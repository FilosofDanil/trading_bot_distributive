"""LiveBroker against a fake exchange: protection orders, foreign symbols, leftovers, the permission check."""
import asyncio
import base64
import hashlib
import hmac

import pytest

from bounce.live import PREFIX, LiveBroker, Rest, check, close_own_position
from bounce.setup import Plan
from bounce.state import Store
from bounce.trader import Slot, Trader


class FakeRest:
    sapi = "sapi"

    def __init__(self, **answers):
        self.calls = []
        self.answers = answers
        self.pos = {}

    async def sync_time(self):
        pass

    async def call(self, method, path, signed=True, base=None, **p):
        self.calls.append((method, path, p))
        key = f"{method} {path}"
        if key == "GET /fapi/v2/positionRisk":
            syms = [p["symbol"]] if "symbol" in p else list(self.pos)
            return [{"symbol": s, "positionAmt": str(self.pos.get(s, 0))} for s in syms]
        if key == "POST /fapi/v1/algoOrder":
            return {"algoStatus": "NEW"}
        if key == "GET /fapi/v1/exchangeInfo":
            return {"symbols": [{"symbol": "XUSDT", "filters": [{"filterType": "LOT_SIZE", "stepSize": "1"}]}]}
        return self.answers.get(key, {} if method != "GET" else [])


def broker_with(rest, tmp_path):
    b = LiveBroker(rest)
    t = Trader(b, Store(tmp_path / "s.json", 0.0), info={"XUSDT": {"tick": 0.0001, "step": 1.0, "min_qty": 1,
                                                                    "min_notional": 5}})
    return b, t


def open_slot(t):
    s = Slot("XUSDT", Plan("long", 1.0002, 0.9985, 1.0050, ("bid", 10000)), 9.0, state="open", filled_qty=9.0,
             entry_px=1.0002)
    t.slots["XUSDT"] = s
    return s


def test_protect_places_exchange_stop_and_reduce_only_take(tmp_path):
    rest = FakeRest()
    b, t = broker_with(rest, tmp_path)
    s = open_slot(t)
    rest.pos["XUSDT"] = 9.0

    async def go():
        await b.protect(s)
        for task in b.tasks:
            task.cancel()
    asyncio.run(go())
    algo = next(p for m, path, p in rest.calls if path == "/fapi/v1/algoOrder")
    assert algo["type"] == "STOP_MARKET" and algo["side"] == "SELL" and algo["closePosition"] == "true"
    assert algo["triggerPrice"] == "0.9985" and algo["clientAlgoId"].startswith(PREFIX)
    take = next(p for m, path, p in rest.calls if path == "/fapi/v1/order" and m == "POST")
    assert take["reduceOnly"] == "true" and take["price"] == "1.0050" and take["quantity"] == "9"
    assert t.store.data["slots"]["XUSDT"]["stop_id"] == s.extra["stop_id"]


def test_foreign_positions_and_orders_are_never_traded(tmp_path):
    rest = FakeRest(**{"GET /fapi/v1/openOrders": [{"symbol": "AUSDT", "clientOrderId": "fc-1-1"},
                                                    {"symbol": "XUSDT", "clientOrderId": PREFIX + "e1"}],
                       "GET /fapi/v1/openAlgoOrders": [{"symbol": "BUSDT", "clientAlgoId": "manual"}]})
    rest.pos = {"CUSDT": -3.0}
    b, t = broker_with(rest, tmp_path)
    asyncio.run(b.refresh_foreign())
    assert {"AUSDT", "BUSDT", "CUSDT", "BTCUSDT"} <= t.blocked_syms and "XUSDT" not in t.blocked_syms
    s = Slot("AUSDT", Plan("long", 1, 0.99, 1.02, ("bid", 1)), 10.0)
    with pytest.raises(RuntimeError, match="foreign"):
        asyncio.run(b.place_entry(s))


def test_leftover_close_never_flips_or_overcloses(tmp_path):
    rest = FakeRest()
    rest.pos = {"XUSDT": -5.0}                                       # a short, but the bot recorded a long
    asyncio.run(close_own_position(rest, "XUSDT", "long", 9.0))
    assert not any(m == "POST" for m, _, _ in rest.calls)
    rest.pos = {"XUSDT": 20.0}                                       # bigger than the bot's own 9
    asyncio.run(close_own_position(rest, "XUSDT", "long", 9.0))
    order = next(p for m, path, p in rest.calls if m == "POST")
    assert order["side"] == "SELL" and order["quantity"] == "9" and order["reduceOnly"] == "true"


def test_check_blocks_read_only_key_and_hedge_mode():
    rest = FakeRest(**{"GET /sapi/v1/account/apiRestrictions": {"enableFutures": False, "enableWithdrawals": True},
                       "GET /fapi/v1/positionSide/dual": {"dualSidePosition": True},
                       "GET /fapi/v2/account": {"availableBalance": "100"}})
    problems = asyncio.run(check(rest))
    assert any("Ed25519" in p for p in problems) and any("withdrawals" in p for p in problems)
    assert any("hedge" in p for p in problems)
    rest = FakeRest(**{"GET /sapi/v1/account/apiRestrictions": {"enableFutures": True},
                       "GET /fapi/v1/positionSide/dual": {"dualSidePosition": False},
                       "GET /fapi/v2/account": {"availableBalance": "100"}})
    assert asyncio.run(check(rest)) == []


def test_signatures():
    r = Rest("k", secret="s")
    assert r.sign("a=1") == hmac.new(b"s", b"a=1", hashlib.sha256).hexdigest()
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    key = Ed25519PrivateKey.generate()
    pem = key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8,
                            serialization.NoEncryption()).decode()
    r = Rest("k", private_pem=pem.replace("\n", "\\n"))              # Railway variables carry \n literally
    key.public_key().verify(base64.b64decode(r.sign("a=1")), b"a=1")   # raises if wrong
    with pytest.raises(SystemExit):
        Rest("k")
