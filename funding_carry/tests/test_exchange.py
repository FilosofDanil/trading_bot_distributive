import base64
import hashlib
import hmac
from urllib.parse import parse_qsl, unquote, urlsplit

import httpx
import pytest
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

from carry import exchange
from carry.exchange import Binance, BinanceError

SECRET = "s3cr3t"


class Recorder:
    def __init__(self, routes):
        self.routes = routes
        self.reqs: list[httpx.Request] = []

    def __call__(self, req: httpx.Request) -> httpx.Response:
        self.reqs.append(req)
        path = urlsplit(str(req.url)).path
        if path.endswith("/time"):
            return httpx.Response(200, json={"serverTime": 1_700_000_000_000})
        out = self.routes.get((req.method, path))
        if callable(out):
            out = out(req)
        if isinstance(out, httpx.Response):
            return out
        return httpx.Response(200, json=out if out is not None else {})

    def params(self, i=-1) -> dict:
        return dict(parse_qsl(urlsplit(str(self.reqs[i].url)).query))


def make(routes, **kw):
    rec = Recorder(routes)
    b = Binance("KEY", http=httpx.Client(transport=httpx.MockTransport(rec)), **kw)
    return b, rec


def test_hmac_signature_and_key_header():
    b, rec = make({("GET", "/fapi/v1/positionSide/dual"): {"dualSidePosition": False}}, secret=SECRET)
    assert b.hedge() is False
    req = rec.reqs[-1]
    raw = urlsplit(str(req.url)).query
    payload, sig = raw.rsplit("&signature=", 1)
    assert hmac.new(SECRET.encode(), payload.encode(), hashlib.sha256).hexdigest() == sig
    assert req.headers["X-MBX-APIKEY"] == "KEY" and "timestamp=" in payload and "recvWindow=10000" in payload


def test_ed25519_signature_is_base64_and_escaped():
    key = Ed25519PrivateKey.generate()
    pem = key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8,
                            serialization.NoEncryption()).decode()
    b, rec = make({("GET", "/fapi/v1/positionSide/dual"): {"dualSidePosition": True}}, private_pem=pem)
    assert b.hedge() is True
    payload, sig = urlsplit(str(rec.reqs[-1].url)).query.rsplit("&signature=", 1)
    assert "+" not in sig and "/" not in sig                 # escaped
    key.public_key().verify(base64.b64decode(unquote(sig)), payload.encode())   # raises if wrong


def test_permissions_check_names_what_is_missing():
    b, _ = make({("GET", "/sapi/v1/account/apiRestrictions"): {
        "enableReading": True, "enableSpotAndMarginTrading": False, "enableFutures": True,
        "permitsUniversalTransfer": False, "enableWithdrawals": True}}, secret=SECRET)
    assert b.check_permissions() == ["spot trading", "universal transfer", "withdrawals must be OFF"]


def test_stop_goes_to_the_algo_service_with_close_position(monkeypatch):
    monkeypatch.setattr(exchange, "perpetuals", lambda: [{"symbol": "XUSDT", "filters": [
        {"filterType": "PRICE_FILTER", "tickSize": "0.0001"}]}])
    b, rec = make({("GET", "/fapi/v1/positionSide/dual"): {"dualSidePosition": True},
                   ("POST", "/fapi/v1/algoOrder"): {"algoId": 77}}, secret=SECRET)
    assert b.place_stop("XUSDT", 2.123456) == "77"
    p = rec.params()
    assert p["algoType"] == "CONDITIONAL" and p["type"] == "STOP_MARKET" and p["side"] == "BUY"
    assert p["closePosition"] == "true" and p["workingType"] == "MARK_PRICE" and p["positionSide"] == "SHORT"
    assert p["triggerPrice"] == "2.1234" and "quantity" not in p and "reduceOnly" not in p
    assert p["clientAlgoId"].startswith("fcs-") and len(p["clientAlgoId"]) <= 36


def test_cancel_stops_only_touches_this_bots_orders():
    b, rec = make({("GET", "/fapi/v1/openAlgoOrders"): [{"algoId": 1, "clientAlgoId": "fcs-1-1"},
                                                        {"algoId": 2, "clientAlgoId": "owner_sl"}],
                   ("DELETE", "/fapi/v1/algoOrder"): {"code": "200"}}, secret=SECRET)
    b.cancel_stops("XUSDT")
    deletes = [r for r in rec.reqs if r.method == "DELETE"]
    assert len(deletes) == 1 and dict(parse_qsl(urlsplit(str(deletes[0].url)).query))["algoId"] == "1"


def test_spot_buy_net_quantity_after_the_coin_fee():
    b, _ = make({("POST", "/api/v3/order"): {"executedQty": "100", "cummulativeQuoteQty": "50", "status": "FILLED",
                                              "fills": [{"commission": "0.1", "commissionAsset": "X"}]}}, secret=SECRET)
    f = b.spot_order("XUSDT", "BUY", 100)
    assert f.qty == pytest.approx(99.9) and f.price == pytest.approx(0.5) and f.fee_usdt == pytest.approx(0.05)


def test_one_way_close_is_reduce_only_hedge_uses_position_side():
    routes = {("GET", "/fapi/v1/positionSide/dual"): {"dualSidePosition": False},
              ("POST", "/fapi/v1/order"): {"executedQty": "10", "avgPrice": "2", "cumQuote": "20"}}
    b, rec = make(routes, secret=SECRET)
    b.perp_order("XUSDT", "BUY", 10, reduce=True)
    assert rec.params()["reduceOnly"] == "true" and "positionSide" not in rec.params()
    b._hedge = True
    b.perp_order("XUSDT", "BUY", 10, reduce=True)
    assert rec.params()["positionSide"] == "SHORT" and "reduceOnly" not in rec.params()


def test_margin_type_already_isolated_is_fine_other_errors_raise():
    calls = {"n": 0}

    def margin(req):
        calls["n"] += 1
        code = -4046 if calls["n"] == 1 else -4048
        return httpx.Response(400, json={"code": code, "msg": "x"})
    b, _ = make({("POST", "/fapi/v1/marginType"): margin, ("POST", "/fapi/v1/leverage"): {"leverage": 1}}, secret=SECRET)
    b.prepare_short("XUSDT")
    with pytest.raises(BinanceError) as e:
        b.prepare_short("XUSDT")
    assert e.value.code == -4048
