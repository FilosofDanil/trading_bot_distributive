import asyncio
import time

import pytest

from bounce import setup
from bounce.book import Trade
from bounce.sim import SimBroker
from bounce.state import Store, utc_day
from bounce.trader import RISK, Slot, Trader
from tests.helpers import PIN_P, PIN_RISK, long_book

NOW = 1000.0
INFO = {"XUSDT": {"tick": 0.0001, "step": 1.0, "min_qty": 1.0, "min_notional": 5.0}}


@pytest.fixture(autouse=True)
def fresh_params():
    """Pinned values (confirm_secs 0 = the old immediate placement; the confirmed entry has its own tests below),
    restored after every test the way tests/test_setup.py does."""
    saved, risk = dict(setup.P), dict(RISK)
    setup.P.update(PIN_P)
    RISK.update(PIN_RISK)
    yield
    setup.P.clear()
    setup.P.update(saved)
    RISK.clear()
    RISK.update(risk)


@pytest.fixture
def parts(monkeypatch):
    """v0.2.0 parts: a quarter at 25 % of the move, break-even when it fills, a runner back to the move's start."""
    monkeypatch.setitem(setup.P, "tp1_r", 0.0)
    monkeypatch.setitem(setup.P, "tp1_frac", 0.25)
    monkeypatch.setitem(setup.P, "tp1_part", 0.25)
    monkeypatch.setitem(setup.P, "be_on_tp1", True)
    monkeypatch.setitem(setup.P, "run_frac", 1.0)


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
    assert [p["name"] for p in s.parts] == ["tp2"]               # 9 USDT: too small to split, one take
    return s


async def open_big_long(t, broker):
    """60 USDT: 59 units split 14 (tp1, 25 % of the move) / 36 (tp2, the take) / 9 (the runner)."""
    await t._seek("XUSDT", t.books["XUSDT"], NOW)
    s = t.slots["XUSDT"]
    broker.on_trade("XUSDT", Trade(NOW + 1, 10001, 100.0, True))
    await settle()
    assert [(p["name"], p["qty"], p["px"]) for p in s.parts] == [("tp1", 14, 1.0100), ("tp2", 36, 1.0200),
                                                                   ("run", 9, 1.0400)]
    return s


def test_one_take_for_the_whole_position_and_no_stop_move(tmp_path, monkeypatch):
    monkeypatch.setitem(RISK, "notional", 60.0)
    monkeypatch.setitem(setup.P, "tp1_r", 0.0)                          # v0.3.0: no part at +1R
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]                                                 # mid 1.0030 = +1.65 R

    async def go():
        await t._seek("XUSDT", b, NOW)
        s = t.slots["XUSDT"]
        broker.on_trade("XUSDT", Trade(NOW + 1, 10001, 100.0, True))
        await settle()
        assert [(p["name"], p["qty"], p["px"]) for p in s.parts] == [("tp2", 59, 1.0200)]
        await t._watch_open(s, b, NOW + 5)
        assert not s.manage.be_done and broker.o["XUSDT"]["stop"] == 9985
        broker.on_trade("XUSDT", Trade(NOW + 30, 10201, 100.0, False))
        await settle()
    asyncio.run(go())
    tr = store.data["trades"][-1]
    assert not t.slots and tr["reason"] == "take" and tr["exit"] == pytest.approx(1.0200)


def test_no_break_even_before_the_first_take(tmp_path, monkeypatch, parts):
    monkeypatch.setitem(RISK, "notional", 60.0)
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]                                                 # mid 1.0030 = +1.65 R

    async def go():
        s = await open_big_long(t, broker)
        await t._watch_open(s, b, NOW + 5)
        assert not s.manage.be_done and broker.o["XUSDT"]["stop"] == 9985
    asyncio.run(go())


def test_partial_takes_break_even_and_runner_lock(tmp_path, monkeypatch, parts):
    monkeypatch.setitem(RISK, "notional", 60.0)
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]

    async def go():
        s = await open_big_long(t, broker)
        broker.on_trade("XUSDT", Trade(NOW + 5, 10101, 50.0, False))     # through tp1
        await settle()
        assert [p["done"] for p in s.parts] == [True, False, False]
        await t._watch_open(s, b, NOW + 5)
        assert s.manage.be_done and broker.o["XUSDT"]["stop"] == 10008   # break-even: entry + 0.06 %
        broker.on_trade("XUSDT", Trade(NOW + 20, 10201, 50.0, False))    # through tp2
        await settle()
        await t._watch_open(s, b, NOW + 20)
        assert s.extra["lock_done"] and broker.o["XUSDT"]["stop"] == 10100   # the runner keeps tp1's profit
        broker.on_trade("XUSDT", Trade(NOW + 40, 10100, 50.0, True))     # back to tp1: the runner stops out
        await settle()
    asyncio.run(go())
    assert not t.slots
    tr = store.data["trades"][-1]
    assert tr["reason"] == "stop_lock" and [p["done"] for p in tr["parts"]] == [True, True, False]
    stop_px = 1.0100 * (1 - 0.0002)
    gross = 14 * (1.0100 - 1.0002) + 36 * (1.0200 - 1.0002) + 9 * (stop_px - 1.0002)
    fees = 59 * 1.0002 * 0.0002 + (14 * 1.0100 + 36 * 1.0200) * 0.0002 + 9 * stop_px * 0.0005
    assert tr["pnl"] == pytest.approx(gross - fees, abs=1e-6) and tr["fees"] == pytest.approx(fees, abs=1e-6)
    assert tr["exit"] == pytest.approx((14 * 1.0100 + 36 * 1.0200 + 9 * stop_px) / 59)
    assert any("closed (stop_lock)" in e["msg"] and "parts tp1 tp2" in e["msg"] for e in store.data["events"])


def test_runner_take_closes_the_last_part(tmp_path, monkeypatch, parts):
    monkeypatch.setitem(RISK, "notional", 60.0)
    t, broker, store = make(tmp_path)

    async def go():
        await open_big_long(t, broker)
        broker.on_trade("XUSDT", Trade(NOW + 30, 10401, 50.0, False))    # one print through every take
        await settle()
    asyncio.run(go())
    tr = store.data["trades"][-1]
    assert not t.slots and tr["reason"] == "take" and all(p["done"] for p in tr["parts"])
    assert tr["exit"] == pytest.approx((14 * 1.0100 + 36 * 1.0200 + 9 * 1.0400) / 59)


def test_partial_then_initial_stop(tmp_path, monkeypatch, parts):
    monkeypatch.setitem(RISK, "notional", 60.0)
    t, broker, store = make(tmp_path)

    async def go():
        await open_big_long(t, broker)
        broker.on_trade("XUSDT", Trade(NOW + 5, 10101, 50.0, False))     # tp1, but the stop is not moved yet
        await settle()
        broker.on_trade("XUSDT", Trade(NOW + 6, 9985, 50.0, True))       # the rest (45) stops out
        await settle()
    asyncio.run(go())
    tr = store.data["trades"][-1]
    stop_px = 0.9985 * (1 - 0.0002)
    gross = 14 * (1.0100 - 1.0002) + 45 * (stop_px - 1.0002)
    fees = 59 * 1.0002 * 0.0002 + 14 * 1.0100 * 0.0002 + 45 * stop_px * 0.0005
    assert tr["reason"] == "stop" and tr["pnl"] == pytest.approx(gross - fees, abs=1e-6)


def test_half_at_one_r_and_the_stop_stays(tmp_path, monkeypatch):
    monkeypatch.setitem(RISK, "notional", 60.0)
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]

    async def go():
        await t._seek("XUSDT", b, NOW)
        s = t.slots["XUSDT"]
        broker.on_trade("XUSDT", Trade(NOW + 1, 10001, 100.0, True))
        await settle()
        assert [(p["name"], p["qty"], p["px"]) for p in s.parts] == [("tp1", 29, 1.0019), ("tp2", 30, 1.0200)]
        broker.on_trade("XUSDT", Trade(NOW + 5, 10020, 50.0, False))     # through tp1 at +1R
        await settle()
        await t._watch_open(s, b, NOW + 5)
        assert s.parts[0]["done"] and not s.manage.be_done and broker.o["XUSDT"]["stop"] == 9985   # no break-even
        broker.on_trade("XUSDT", Trade(NOW + 9, 9985, 50.0, True))       # the rest stops out where it started
        await settle()
    asyncio.run(go())
    tr = store.data["trades"][-1]
    stop_px = 0.9985 * (1 - 0.0002)
    gross = 29 * (1.0019 - 1.0002) + 30 * (stop_px - 1.0002)
    fees = 59 * 1.0002 * 0.0002 + 29 * 1.0019 * 0.0002 + 30 * stop_px * 0.0005
    assert tr["reason"] == "stop" and tr["pnl"] == pytest.approx(gross - fees, abs=1e-6)
    assert tr["slip"] is None                                            # the exit price averages in the part


def test_size_keeps_the_usdt_risk(tmp_path, monkeypatch):
    t, broker, store = make(tmp_path)
    monkeypatch.setitem(RISK, "notional", 60.0)
    assert t.size("XUSDT", 1.0002, 0.9985) == 59.0                      # default risk 0.12 USDT: the notional caps it
    monkeypatch.setitem(RISK, "risk_usd", 0.05)
    assert t.size("XUSDT", 1.0002, 0.9985) == 29.0                      # 0.05 USDT / 0.17 % = 29.4 USDT
    assert t.size("XUSDT", 1.0002, 0.9902) == 5.0                       # 1 % stop: 5 USDT (the exchange minimum)
    assert t.size("XUSDT", 1.0002) == 59.0                              # no stop: the notional


def test_no_entry_when_the_book_behind_the_stop_is_thin(tmp_path):
    t, broker, store = make(tmp_path)
    t.books["XUSDT"] = long_book(NOW, depth=1.0)                        # 7 units within 0.3 % beyond the stop < 3 x 9
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW))
    assert not t.slots and store.data["stats"]["thin_stop"] == 1 and t.cool["XUSDT"] == NOW + 30
    t.cool.clear()
    t.books["XUSDT"] = long_book(NOW, depth=4.0)                        # 28 >= 27
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW))
    assert t.slots["XUSDT"].plan.features["depth_x"] == pytest.approx(3.1, abs=0.05)


def test_two_slipped_stops_ban_the_symbol_for_a_day(tmp_path):
    t, broker, store = make(tmp_path)

    def stopped(exit_px):
        t.slots["XUSDT"] = Slot("XUSDT", setup.Plan("long", 1.0002, 0.9985, 1.0200, ("bid", 10000)), 9.0,
                                state="open", filled_qty=9.0, entry_px=1.0002, opened_at=time.time(), stop=0.9985)
        t.on_closed("XUSDT", exit_px, -0.05, 0.01, "stop")
        return store.data["trades"][-1]
    assert stopped(0.9980)["slip"] == pytest.approx(0.0005, abs=1e-5)  # 0.05 %: an ordinary fill
    assert stopped(0.9950)["slip"] == pytest.approx(0.0035 / 0.9985, abs=1e-5)   # 0.35 %: the first slip
    assert t.cool["XUSDT"] < time.time() + 120
    stopped(0.9950)                                                      # the second within 24 h: banned
    assert t.cool["XUSDT"] > time.time() + 86_000
    assert any("no trades on it for 24 h" in e["msg"] for e in store.data["events"])


def test_take_at_middle_of_the_move(tmp_path):
    t, broker, store = make(tmp_path)

    async def go():
        await open_long(t, broker)
        broker.on_trade("XUSDT", Trade(NOW + 30, 10201, 50.0, False))   # a buy prints through the take
        await settle()
    asyncio.run(go())
    assert not t.slots
    tr = store.data["trades"][-1]
    assert tr["reason"] == "take" and tr["exit"] == pytest.approx(1.0200)
    gross = 9 * (1.0200 - 1.0002)
    assert tr["pnl"] == pytest.approx(gross - 9 * 1.0002 * 0.0002 - 9 * 1.0200 * 0.0002, abs=1e-6)
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
    assert tr["slip"] == pytest.approx(0.0002, abs=1e-6)
    assert store.data["consec_losses"] == 1


def test_fee_rebate_counts_in_the_journal_and_the_totals(tmp_path, monkeypatch):
    monkeypatch.setitem(RISK, "fee_rebate", 0.4)
    t, broker, store = make(tmp_path)

    async def go():
        await open_long(t, broker)
        broker.on_trade("XUSDT", Trade(NOW + 5, 9985, 50.0, True))
        await settle()
    asyncio.run(go())
    tr = store.data["trades"][-1]
    exit_px = 0.9985 * (1 - 0.0002)
    paid = 9 * 1.0002 * 0.0002 + 9 * exit_px * 0.0005
    assert tr["rebate"] == pytest.approx(0.4 * paid, abs=1e-6) and tr["fees"] == pytest.approx(0.6 * paid, abs=1e-6)
    assert tr["pnl"] == pytest.approx(9 * (exit_px - 1.0002) - 0.6 * paid, abs=1e-6)
    assert store.data["totals"]["pnl"] == pytest.approx(tr["pnl"], abs=1e-6)
    assert store.data["totals"]["fees"] == pytest.approx(tr["fees"], abs=1e-6)


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


def test_raising_the_consec_limit_lifts_todays_block(tmp_path, monkeypatch):
    monkeypatch.setitem(RISK, "max_consec", 1)
    t, broker, store = make(tmp_path)

    async def go():
        await open_long(t, broker)
        broker.on_trade("XUSDT", Trade(NOW + 5, 9985, 50.0, True))      # the stop: one loss hits the limit of 1
        await settle()
    asyncio.run(go())
    assert store.data["consec_block_limit"] == 1
    store.data["consec_block_day"] = utc_day(NOW)                       # the trade closed on the wall clock's day
    assert t.block(NOW) == "1 losses in a row today"
    monkeypatch.setitem(RISK, "max_consec", 12)
    assert t.block(NOW) == ""
    monkeypatch.setitem(RISK, "max_consec", 1)                          # same limit after a restart: still blocked
    assert "in a row" in t.block(NOW)
    del store.data["consec_block_limit"]                                # a block recorded before the limit was kept
    assert t.block(NOW) == ""


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
