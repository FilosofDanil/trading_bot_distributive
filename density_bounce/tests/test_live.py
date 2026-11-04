"""LiveBroker against a fake exchange: protection orders, foreign symbols, leftovers, the permission check."""
import asyncio
import base64
import hashlib
import hmac

import pytest

from bounce.live import PREFIX, BinanceError, LiveBroker, Rest, check, close_own_position
from bounce.setup import Plan
from bounce.state import Store
from bounce.trader import Slot, Trader


class FakeRest:
    sapi = "sapi"

    def __init__(self, **answers):
        self.calls = []
        self.answers = answers
        self.pos = {}
        self.orders = {}                       # client id -> GET /fapi/v1/order answer
        self.pos_seq = []                      # scripted positionAmt readings for XUSDT (then self.pos)
        self.stops = {}                        # open algo stops: client id -> trigger price
        self.stop_errors = []                  # errors for the next POST /fapi/v1/algoOrder calls

    async def sync_time(self):
        pass

    async def call(self, method, path, signed=True, base=None, **p):
        self.calls.append((method, path, p))
        key = f"{method} {path}"
        if key == "GET /fapi/v2/positionRisk":
            if self.pos_seq and p.get("symbol") == "XUSDT":
                self.pos["XUSDT"] = self.pos_seq.pop(0)
            syms = [p["symbol"]] if "symbol" in p else list(self.pos)
            return [{"symbol": s, "positionAmt": str(self.pos.get(s, 0))} for s in syms]
        if key == "GET /fapi/v1/order":
            return self.orders.get(p["origClientOrderId"], {"status": "NEW", "executedQty": "0"})
        if key == "POST /fapi/v1/algoOrder":
            if self.stop_errors:
                raise self.stop_errors.pop(0)
            if self.stops:                                 # like Binance: one closePosition stop per side
                raise BinanceError(-4130, "An open stop or take profit order with GTE and closePosition is existing")
            self.stops[p["clientAlgoId"]] = p["triggerPrice"]
            return {"algoStatus": "NEW"}
        if key == "DELETE /fapi/v1/algoOrder":
            self.stops.pop(p["clientAlgoId"], None)
            return {}
        if key == "GET /fapi/v1/exchangeInfo":
            return {"symbols": [{"symbol": "XUSDT", "filters": [{"filterType": "LOT_SIZE", "stepSize": "1"}]}]}
        return self.answers.get(key, {} if method != "GET" else [])


def broker_with(rest, tmp_path):
    b = LiveBroker(rest)
    t = Trader(b, Store(tmp_path / "s.json", 0.0), info={"XUSDT": {"tick": 0.0001, "step": 1.0, "min_qty": 1,
                                                                    "min_notional": 5}})
    return b, t


def open_slot(t, qty=9.0):
    s = Slot("XUSDT", Plan("long", 1.0002, 0.9985, 1.0050, ("bid", 10000), tp1=1.0019, tp_run=1.0100), qty,
             state="open", filled_qty=qty, entry_px=1.0002)
    s.parts = t.split(s)
    t.slots["XUSDT"] = s
    return s


def test_takes_one_reduce_only_order_per_part(tmp_path):
    rest = FakeRest()
    b, t = broker_with(rest, tmp_path)
    s = open_slot(t, 59.0)
    rest.pos["XUSDT"] = 59.0

    async def go():
        await b.protect(s)
        for task in b.tasks:
            task.cancel()
    asyncio.run(go())
    takes = [p for m, path, p in rest.calls if path == "/fapi/v1/order" and m == "POST"]
    assert [(o["price"], o["quantity"]) for o in takes] == [("1.0019", "14"), ("1.0050", "36"), ("1.0100", "9")]
    assert all(o["reduceOnly"] == "true" for o in takes)
    assert t.store.data["slots"]["XUSDT"]["tp_ids"] == [p["id"] for p in s.parts]


def test_poll_reports_parts_and_cancels_the_rest(tmp_path, monkeypatch):
    rest = FakeRest()
    b, t = broker_with(rest, tmp_path)
    s = open_slot(t, 59.0)
    for p, cid in zip(s.parts, ("t1", "t2", "t3")):
        p["id"] = cid
    rest.orders["t1"] = {"status": "FILLED", "executedQty": "14", "avgPrice": "1.0019"}
    rest.pos_seq = [59.0, 45.0, 45.0, 0.0, 0.0]                      # tp1 fills, then the stop closes the rest
    sleep = asyncio.sleep
    monkeypatch.setattr("bounce.live.asyncio.sleep", lambda _: sleep(0))
    asyncio.run(b._poll_open(s))
    tr = t.store.data["trades"][-1]
    assert tr["reason"] == "stop" and [p["done"] for p in tr["parts"]] == [True, False, False]
    cancelled = {p["origClientOrderId"] for m, path, p in rest.calls if m == "DELETE" and path == "/fapi/v1/order"}
    assert cancelled == {"t2", "t3"}


def stopped_slot(rest, t):
    s = open_slot(t, 59.0)
    s.stop = s.plan.stop
    s.extra["stop_id"] = "old"
    rest.stops["old"] = "0.9985"
    return s


def test_move_stop_replaces_the_only_close_position_stop(tmp_path):
    rest = FakeRest()
    b, t = broker_with(rest, tmp_path)
    s = stopped_slot(rest, t)
    asyncio.run(t._move_stop(s, 1.0008, "stop_be"))
    assert list(rest.stops.values()) == ["1.0008"] and s.extra["stop_id"] in rest.stops
    assert s.stop == 1.0008 and s.extra["stop_why"] == "stop_be"
    assert t.store.data["slots"]["XUSDT"]["stop_id"] == s.extra["stop_id"]


def test_move_stop_failure_restores_the_old_stop(tmp_path):
    rest = FakeRest()
    b, t = broker_with(rest, tmp_path)
    s = stopped_slot(rest, t)
    rest.stop_errors = [BinanceError(-1008, "overloaded")]
    asyncio.run(t._move_stop(s, 1.0008, "stop_be"))
    assert list(rest.stops.values()) == ["0.9985"] and not s.close_sent
    assert s.stop == s.plan.stop and "stop_why" not in s.extra       # a later stop-out is the initial stop
    assert any("move stop" in e["msg"] for e in t.store.data["events"])


def test_move_stop_closes_when_no_stop_can_be_placed(tmp_path):
    rest = FakeRest()
    b, t = broker_with(rest, tmp_path)
    s = stopped_slot(rest, t)
    rest.pos["XUSDT"] = 59.0
    rest.stop_errors = [BinanceError(-1008, "overloaded"), BinanceError(-1008, "overloaded")]
    asyncio.run(t._move_stop(s, 1.0008, "stop_be"))
    assert s.close_sent and s.extra["exit_reason"] == "stop_lost"
    market = next(p for m, path, p in rest.calls if m == "POST" and p.get("type") == "MARKET")
    assert market["side"] == "SELL" and market["reduceOnly"] == "true"


def test_move_stop_past_the_price_closes_at_market(tmp_path):
    rest = FakeRest()
    b, t = broker_with(rest, tmp_path)
    s = stopped_slot(rest, t)
    rest.pos["XUSDT"] = 59.0
    rest.stop_errors = [BinanceError(-2021, "would immediately trigger")]
    asyncio.run(t._move_stop(s, 1.0008, "stop_be"))
    assert s.close_sent and s.extra["exit_reason"] == "stop_be" and not rest.stops


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


def test_check_needs_the_margin_of_all_trades(monkeypatch):
    from bounce import trader
    monkeypatch.setitem(trader.RISK, "notional", 500.0)              # 2 trades x 500 at x2 = 500 USDT margin
    ok = {"GET /sapi/v1/account/apiRestrictions": {"enableFutures": True},
          "GET /fapi/v1/positionSide/dual": {"dualSidePosition": False}}
    rest = FakeRest(**ok, **{"GET /fapi/v2/account": {"availableBalance": "499"}})
    assert any("< 500 USDT" in p for p in asyncio.run(check(rest)))
    rest = FakeRest(**ok, **{"GET /fapi/v2/account": {"availableBalance": "600"}})
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
