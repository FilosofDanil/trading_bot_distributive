import asyncio

import pytest

from bounce.book import Trade
from bounce.sim import SimBroker
from bounce.state import Store, utc_day
from bounce.trader import RISK, Trader
from tests.helpers import long_book

NOW = 1000.0
INFO = {"XUSDT": {"tick": 0.0001, "step": 1.0, "min_qty": 1.0, "min_notional": 5.0}}


def make(tmp_path, desired="running"):
    store = Store(tmp_path / "s.json", 100.0)
    store.data["desired"] = desired
    broker = SimBroker()
    t = Trader(broker, store, info=dict(INFO))
    t.books["XUSDT"] = long_book(NOW)
    return t, broker, store


async def settle():
    for _ in range(3):
        await asyncio.sleep(0)


async def open_long(t, broker):
    await t._seek("XUSDT", t.books["XUSDT"], NOW)
    s = t.slots["XUSDT"]
    assert s.state == "entry" and s.qty == 9.0                   # 10 USDT / 1.0002, step 1
    broker.on_trade("XUSDT", Trade(NOW + 1, 10001, 50.0, True))   # a sell prints through our bid at 1.0002
    await settle()
    assert s.state == "open" and s.entry_px == pytest.approx(1.0002)
    assert broker.o["XUSDT"]["kind"] == "open"                   # stop + take resting
    return s


def test_take_at_middle_of_the_move(tmp_path):
    t, broker, store = make(tmp_path)

    async def go():
        await open_long(t, broker)
        broker.on_trade("XUSDT", Trade(NOW + 30, 10051, 50.0, False))   # a buy prints through the take
        await settle()
    asyncio.run(go())
    assert not t.slots
    tr = store.data["trades"][-1]
    assert tr["reason"] == "take" and tr["exit"] == pytest.approx(1.0050)
    gross = 9 * (1.0050 - 1.0002)
    assert tr["pnl"] == pytest.approx(gross - 9 * 1.0002 * 0.0002 - 9 * 1.0050 * 0.0002, abs=1e-6)
    assert store.data["totals"]["n"] == 1 and store.data["totals"]["wins"] == 1
    assert any(e["notify"] and "closed (take)" in e["msg"] for e in store.data["events"])


def test_stop_behind_the_density(tmp_path):
    t, broker, store = make(tmp_path)

    async def go():
        await open_long(t, broker)
        broker.on_trade("XUSDT", Trade(NOW + 5, 9985, 50.0, True))      # trades at the stop
        await settle()
    asyncio.run(go())
    tr = store.data["trades"][-1]
    assert tr["reason"] == "stop" and tr["pnl"] < 0
    exit_px = 0.9985 * (1 - 0.0002)                                  # stop price minus 0.02 % slippage
    pnl = 9 * (exit_px - 1.0002) - 9 * 1.0002 * 0.0002 - 9 * exit_px * 0.0005
    assert tr["exit"] == pytest.approx(exit_px) and tr["pnl"] == pytest.approx(pnl, abs=1e-6)
    assert tr["r"] == pytest.approx(pnl / (9 * 0.0017), abs=0.01)     # about -1.5 R: fees + slippage add ~0.5 R
    assert store.data["consec_losses"] == 1


def test_entry_cancelled_when_density_goes(tmp_path):
    t, broker, store = make(tmp_path)

    async def go():
        await t._seek("XUSDT", t.books["XUSDT"], NOW)
        t.books["XUSDT"].densities[("bid", 10000)].gone = "pulled"
        await t._watch_entry(t.slots["XUSDT"], t.books["XUSDT"], NOW + 1)
    asyncio.run(go())
    assert not t.slots and store.data["stats"]["cancelled"] == 1 and not store.data["trades"]


def test_pause_and_risk_blocks(tmp_path):
    t, broker, store = make(tmp_path, desired="paused")
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW))
    assert not t.slots                                             # paused: no new entries

    store.data["desired"] = "running"
    store.data["days"][utc_day(NOW)] = {"n": 3, "wins": 0, "pnl": -RISK["day_loss"], "gross": 0, "fees": 0, "r": 0}
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW))
    assert not t.slots and "day loss" in t.last_block

    store.data["days"] = {}
    store.data["totals"]["pnl"] = -RISK["total_loss"]
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW))
    assert not t.slots and "halted" in t.halt


def test_flatten_closes_open_trade(tmp_path):
    t, broker, store = make(tmp_path)

    async def go():
        await open_long(t, broker)
        assert await t.flatten() == 1
    asyncio.run(go())
    assert not t.slots and store.data["trades"][-1]["reason"] == "flatten"


def test_tick_runs_on_a_synced_book(tmp_path):
    t, broker, store = make(tmp_path)
    t.books["XUSDT"].last_ts = NOW
    asyncio.run(t.tick(NOW))
    assert "XUSDT" in t.slots                                      # scan + seek through the public entry point
