"""v0.4.5 of trader.py / sim.py / state.py: confirmed entry, _entry_over, shadow journal, market, risk, sim lag."""
import asyncio
import json
import time

import pytest

import bounce
from bounce import setup
from bounce.book import Trade
from bounce.runner import Runner
from bounce.sim import SimBroker
from bounce.state import Store, utc_day
from bounce.trader import RISK, Slot, Trader
from tests.helpers import PIN_P, PIN_RISK, long_book

NOW = 1000.0
T0 = 100_000.0
INFO = {"XUSDT": {"tick": 0.0001, "step": 1.0, "min_qty": 1.0, "min_notional": 5.0}}
KEY = ("XUSDT", ("bid", 10000))


@pytest.fixture(autouse=True)
def fresh_params():
    saved, risk = dict(setup.P), dict(RISK)
    setup.P.update(PIN_P)
    RISK.update(PIN_RISK)
    yield
    setup.P.clear()
    setup.P.update(saved)
    RISK.clear()
    RISK.update(risk)


def make(tmp_path, lag=0.0, book=None, now=NOW):
    store = Store(tmp_path / "s.json", 100.0)
    broker = SimBroker(lag=lag)
    t = Trader(broker, store, info=dict(INFO))
    t.books["XUSDT"] = book or long_book(now)
    return t, broker, store


async def settle():
    for _ in range(3):
        await asyncio.sleep(0)


def stat(store, key):
    return store.data["stats"].get(key, 0)


def sells(b, ts, usd, tick=10030):
    b.trades.append(Trade(ts, tick, usd / (tick * b.tick_size), True))


def touch(b, top=10002):
    """Price comes down to the entry level: no bid is left above `top`."""
    b.bids = {k: q for k, q in b.bids.items() if k <= top}


def test_version_and_defaults():
    assert bounce.VERSION == "0.4.5"
    assert Trader(SimBroker(), Store.__new__(Store)).live is False


# ------------------------------------------------------------------ the confirmed entry
def test_armed_entry_is_placed_15_s_after_the_touch(tmp_path):
    setup.P["confirm_secs"] = 15.0
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]

    async def go():
        await t._seek("XUSDT", b, NOW)
        s = t.slots["XUSDT"]
        assert s.state == "arm" and "XUSDT" not in broker.o         # no order at the plan
        assert stat(store, "armed") == 1 and stat(store, "placed") == 0
        await t._watch_arm(s, b, NOW + 1)
        assert "touch_at" not in s.extra                              # price has not reached the entry level
        touch(b)
        await t._watch_arm(s, b, NOW + 2)
        assert s.extra["touch_at"] == NOW + 2
        await t._watch_arm(s, b, NOW + 16.9)                          # 14.9 s after the touch: still waiting
        assert s.state == "arm" and "XUSDT" not in broker.o
        await t._watch_arm(s, b, NOW + 17)                            # 15 s
        assert s.state == "entry" and s.placed_at == NOW + 17 and broker.o["XUSDT"]["kind"] == "entry"
        assert stat(store, "placed") == 1 and s.plan.features["armed_s"] == 17.0
        broker.on_trade("XUSDT", Trade(NOW + 18, 10001, 50.0, True))
        await settle()
        assert s.state == "open" and s.entry_px == pytest.approx(1.0002)
    asyncio.run(go())


def test_confirm_secs_zero_places_the_order_at_the_plan(tmp_path):
    setup.P["confirm_secs"] = 0.0
    t, broker, store = make(tmp_path)
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW))
    s = t.slots["XUSDT"]
    assert s.state == "entry" and broker.o["XUSDT"]["kind"] == "entry" and stat(store, "placed") == 1
    assert stat(store, "armed") == 0


def test_the_order_goes_one_tick_below_the_best_ask_when_the_entry_would_cross(tmp_path):
    setup.P["confirm_secs"] = 15.0
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]

    async def go():
        await t._seek("XUSDT", b, NOW)
        s = t.slots["XUSDT"]
        touch(b, 10001)
        b.asks = {10002: 100.0, 10003: 100.0}                         # the ask came down to 1.0002 = the entry
        await t._watch_arm(s, b, NOW + 1)
        await t._watch_arm(s, b, NOW + 16)
        assert s.state == "entry" and s.plan.entry == pytest.approx(1.0001)
        assert broker.o["XUSDT"]["tick"] == 10001
    asyncio.run(go())


def test_unconfirmed_density_cancels_the_entry_blocks_it_and_is_journaled(tmp_path):
    setup.P["confirm_secs"] = 15.0
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]
    d = b.densities[("bid", 10000)]

    async def go():
        await t._seek("XUSDT", b, NOW)
        s = t.slots["XUSDT"]
        touch(b)
        await t._watch_arm(s, b, NOW + 2)
        d.usd = 0.65 * d.peak_usd                                     # under 70 % at the confirmation, above 50 %
        await t._watch_arm(s, b, NOW + 17)
        return s
    s = asyncio.run(go())
    assert not t.slots and "XUSDT" not in broker.o                    # never an order on the exchange
    assert stat(store, "cancel_unconfirmed") == 1 and stat(store, "cancelled") == 1
    assert t.failed[KEY] == NOW + 17 + RISK["wall_cool"] and t.cool["XUSDT"] == NOW + 17 + 5
    # the plain entry would have been filled and stopped: the journal shows what the cancel saved
    b.trades.append(Trade(NOW + 20, 10001, 50.0, True))
    b.trades.append(Trade(NOW + 25, 9985, 50.0, True))
    t._step_shadows(NOW + 30)
    rec = store.plans()[-1]
    assert rec["act"] == "cancel:unconfirmed" and rec["pid"] == s.plan.pid and rec["ver"] == bounce.VERSION
    assert rec["shadow"]["hit"] == "stop" and rec["shadow"]["r"] < -1.0 and rec["skip"] == ""
    # the density is blocked for wall_cool, a healthy one is not
    d.usd = d.peak_usd
    t.cool.clear()
    t.books["XUSDT"] = long_book(NOW)
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW + 10))
    assert "XUSDT" not in t.slots
    t.failed[KEY] = NOW + 10                                          # expired by then
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW + 10))
    assert "XUSDT" in t.slots


def test_an_intact_density_is_confirmed_even_if_it_lost_a_little(tmp_path):
    setup.P["confirm_secs"] = 15.0
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]
    d = b.densities[("bid", 10000)]

    async def go():
        await t._seek("XUSDT", b, NOW)
        s = t.slots["XUSDT"]
        touch(b)
        await t._watch_arm(s, b, NOW + 2)
        d.usd = 0.75 * d.peak_usd
        await t._watch_arm(s, b, NOW + 17)
        assert s.state == "entry"
    asyncio.run(go())
    assert not t.failed


def test_armed_entry_without_a_touch_times_out_without_blocking_the_density(tmp_path):
    setup.P["confirm_secs"] = 15.0
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]

    async def go():
        await t._seek("XUSDT", b, NOW)
        await t._watch_arm(t.slots["XUSDT"], b, NOW + 100)
        assert "XUSDT" in t.slots
        await t._watch_arm(t.slots["XUSDT"], b, NOW + 181)
    asyncio.run(go())
    assert not t.slots and "XUSDT" not in broker.o and not t.failed
    assert stat(store, "cancel_entry_timeout") == 1 and t.cool["XUSDT"] == NOW + 181 + RISK["sym_cooldown"]


def test_armed_entry_follows_cancel_reason_halt_pause_and_stale_book(tmp_path):
    setup.P["confirm_secs"] = 15.0
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]
    asyncio.run(t._seek("XUSDT", b, NOW))
    t.halt = "total loss"
    asyncio.run(t._watch_arm(t.slots["XUSDT"], b, NOW + 1))
    assert not t.slots and stat(store, "cancel_halt") == 1
    t.halt, t.cool = "", {}
    asyncio.run(t._seek("XUSDT", b, NOW))
    store.data["desired"] = "paused"
    asyncio.run(t._watch_arm(t.slots["XUSDT"], b, NOW + 1))
    assert not t.slots and stat(store, "cancel_paused") == 1
    store.data["desired"], t.cool = "running", {}
    t.attempts.clear()
    asyncio.run(t._seek("XUSDT", b, NOW))
    asyncio.run(t.tick(NOW + 10))                                     # the book is 10 s old: blind
    assert not t.slots and stat(store, "cancel_stale_book") == 1
    t.cool = {}
    t.attempts.clear()
    asyncio.run(t._seek("XUSDT", b, NOW))
    assert asyncio.run(t.flatten()) == 1 and not t.slots and stat(store, "cancel_flatten") == 1


def test_armed_entry_with_a_weak_wall_is_cancelled_as_density_weak(tmp_path):
    setup.P["confirm_secs"] = 15.0
    setup.P["hold_cancel"] = 30.0
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]
    asyncio.run(t._seek("XUSDT", b, NOW))
    sells(b, NOW - 2, 400_000)                                        # 6.7k USDT/s against a 100k wall: 15 s worth
    asyncio.run(t._watch_arm(t.slots["XUSDT"], b, NOW + 1))
    assert not t.slots and stat(store, "cancel_density_weak") == 1 and KEY in t.failed


# ------------------------------------------------------------------ _entry_over
@pytest.mark.parametrize("why", list(setup.WALL_FAILED) + ["price_away", "entry_timeout", "counter_stalled",
                                                           "rejected", "stale_book", "flatten", "expired"])
def test_entry_over_counts_journals_and_sets_the_cooldown(tmp_path, why):
    t, broker, store = make(tmp_path)
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW))
    s = t.slots["XUSDT"]
    t._entry_over(s, why, NOW)
    assert not t.slots and stat(store, "cancelled") == 1 and stat(store, f"cancel_{why}") == 1
    sh = t.shadows[s.plan.pid]
    assert sh.act == f"cancel:{why}" and sh.final
    if why in setup.WALL_FAILED:
        assert t.failed[KEY] == NOW + RISK["wall_cool"] and t.cool["XUSDT"] == NOW + 5
    else:
        assert KEY not in t.failed
        assert t.cool["XUSDT"] == NOW + (30 if why == "rejected" else RISK["sym_cooldown"])


def test_a_working_entry_cancelled_because_the_wall_went_blocks_that_wall(tmp_path):
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]

    async def go():
        await t._seek("XUSDT", b, NOW)
        b.densities[("bid", 10000)].gone = "pulled"
        await t._watch_entry(t.slots["XUSDT"], b, NOW + 1)
    asyncio.run(go())
    assert not t.slots and stat(store, "cancel_density_gone") == 1
    assert t.failed[KEY] > time.time() + RISK["wall_cool"] - 60
    t.cool.clear()
    b2 = long_book(NOW)
    asyncio.run(t._seek("XUSDT", b2, NOW + 10))
    assert not t.slots                                                # the same wall is not traded again


def test_a_rejected_entry_is_cancelled_with_a_short_cooldown(tmp_path, monkeypatch):
    t, broker, store = make(tmp_path)

    async def boom(s):
        raise RuntimeError("post-only would cross")
    monkeypatch.setattr(broker, "place_entry", boom)
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW))
    assert not t.slots and stat(store, "rejected") == 1 and stat(store, "cancel_rejected") == 1
    assert t.cool["XUSDT"] == NOW + 30 and not t.failed


def test_a_working_entry_that_expires_unfilled_is_cancelled_as_expired(tmp_path):
    t, broker, store = make(tmp_path)

    async def go():
        await t._seek("XUSDT", t.books["XUSDT"], NOW)
        await broker.cancel_entry(t.slots["XUSDT"])                   # no reason recorded
    asyncio.run(go())
    assert not t.slots and stat(store, "cancel_expired") == 1


# ------------------------------------------------------------------ the plan journal
def test_skipped_plan_is_journaled_with_its_shadow_outcome(tmp_path):
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]
    sells(b, NOW - 30, 120_000)                                       # a weak wall: 50 s of the flow
    asyncio.run(t._seek("XUSDT", b, NOW))
    assert not t.slots and stat(store, "plans") == 0                  # nothing traded, not even counted
    assert len(t.shadows) == 1
    sh = next(iter(t.shadows.values()))
    assert sh.act == "skip:weak" and sh.final and sh.plan.pid.startswith("XUSDT-")
    b.trades.append(Trade(NOW + 1, 10001, 50.0, True))                # the plain entry would have been filled ...
    b.trades.append(Trade(NOW + 5, 9985, 50.0, True))                 # ... and stopped
    t._step_shadows(NOW + 6)
    assert not t.shadows
    [rec] = store.plans()
    assert rec["act"] == "skip:weak" and rec["skip"] == "weak" and rec["ver"] == bounce.VERSION
    assert rec["shadow"]["hit"] == "stop" and rec["shadow"]["r"] < -1.0 and rec["features"]["hold_s"] == pytest.approx(50, abs=0.2)
    assert {"pid", "sym", "side", "entry", "stop", "tp", "tp1", "ts"} <= set(rec)
    assert store.plans_path.name == "s_plans.jsonl"


def test_a_strong_wall_trades_and_a_skipped_plan_without_a_fill_is_a_nofill(tmp_path):
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]
    sells(b, NOW - 30, 30_000)                                        # 200 s worth: strong
    asyncio.run(t._seek("XUSDT", b, NOW))
    s = t.slots["XUSDT"]
    assert s.plan.features["hold_s"] == pytest.approx(200, abs=0.5) and s.plan.skip == ""
    assert t.shadows[s.plan.pid].act == "open" and not t.shadows[s.plan.pid].final
    t2, _, store2 = make(tmp_path / "b")
    b2 = t2.books["XUSDT"]
    sells(b2, NOW - 30, 120_000)
    asyncio.run(t2._seek("XUSDT", b2, NOW))
    t2._step_shadows(NOW + 200)                                       # the tape never went through the entry
    assert store2.plans()[-1]["shadow"] == {"hit": "nofill", "r": None}


def test_young_and_market_skips_and_thin_stop_are_journaled(tmp_path):
    t, broker, store = make(tmp_path, book=long_book(NOW, age=30.0))
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW))
    assert not t.slots and next(iter(t.shadows.values())).act == "skip:young"
    t, broker, store = make(tmp_path / "c", book=long_book(NOW, depth=1.0))
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW))
    sh = next(iter(t.shadows.values()))
    assert not t.slots and sh.act == "skip:thin_stop" and sh.plan.skip == "thin_stop"
    assert stat(store, "thin_stop") == 1


def test_skipped_plans_are_rate_limited_per_density_and_gate_set(tmp_path):
    t, broker, store = make(tmp_path)
    p, ghost = setup.scan(long_book(NOW, age=30.0), NOW, set())
    t._shadow("XUSDT", ghost, NOW, "skip:young", True)
    assert len(t.shadows) == 1
    t._shadow("XUSDT", ghost, NOW + 599, "skip:young", True)          # the same density and gates within 10 min
    assert len(t.shadows) == 1
    t._shadow("XUSDT", ghost, NOW + 601, "skip:young", True)
    assert len(t.shadows) == 2
    ghost2 = setup.scan(long_book(NOW, age=30.0), NOW, set())[1]
    ghost2.skip = "young+weak"                                        # other gates: its own timer
    t._shadow("XUSDT", ghost2, NOW + 602, "skip:young+weak", True)
    assert len(t.shadows) == 3
    plan = setup.scan(long_book(NOW), NOW, set())[0]                  # a traded plan is never rate-limited
    t._shadow("XUSDT", plan, NOW + 603, "open", False)
    t._shadow("XUSDT", plan, NOW + 604, "open", False)
    assert len(t.shadows) == 5


def test_a_traded_plan_is_journaled_as_filled_and_the_trade_carries_ver_and_pid(tmp_path):
    t, broker, store = make(tmp_path)
    b = t.books["XUSDT"]

    async def go():
        await t._seek("XUSDT", b, NOW)
        s = t.slots["XUSDT"]
        fill = Trade(NOW + 1, 10001, 50.0, True)
        b.trades.append(fill)
        broker.on_trade("XUSDT", fill)
        await settle()
        assert s.state == "open" and t.shadows[s.plan.pid].act == "filled" and t.shadows[s.plan.pid].final
        take = Trade(NOW + 30, 10201, 50.0, False)
        b.trades.append(take)
        broker.on_trade("XUSDT", take)
        await settle()
        return s
    s = asyncio.run(go())
    tr = store.data["trades"][-1]
    assert tr["ver"] == "0.4.5" and tr["pid"] == s.plan.pid and tr["pid"].startswith("XUSDT-")
    t._step_shadows(NOW + 31)
    rec = store.plans()[-1]
    assert rec["pid"] == tr["pid"] and rec["act"] == "filled" and rec["shadow"]["hit"] == "take"
    assert rec["shadow"]["r"] > 5 and not t.shadows


def test_the_feed_keeps_watching_a_symbol_with_a_shadow_plan(tmp_path):
    t, broker, store = make(tmp_path)
    assert t.keep() == set()
    t._shadow("XUSDT", setup.scan(long_book(NOW), NOW, set())[0], NOW, "open", False)
    assert t.keep() == {"XUSDT"}


def test_a_shadow_of_a_symbol_that_left_the_watch_list_is_lost(tmp_path):
    t, broker, store = make(tmp_path)
    t._shadow("XUSDT", setup.scan(long_book(NOW), NOW, set())[0], NOW, "cancel:price_away", True)
    del t.books["XUSDT"]
    t._step_shadows(NOW + 5)
    assert store.plans()[-1]["shadow"] == {"hit": "lost", "r": None} and not t.shadows


# ------------------------------------------------------------------ Store.plan / plans
def test_store_plans_tail_corrupt_lines_and_rotation(tmp_path):
    store = Store(tmp_path / "state.json", 100.0)
    assert store.plans() == [] and store.plans_path.name == "state_plans.jsonl"
    for i in range(5):
        store.plan({"pid": f"p{i}", "n": i})
    with store.plans_path.open("a", encoding="utf-8") as f:
        f.write("not json\n")
    assert [r["pid"] for r in store.plans(3)] == ["p3", "p4"]         # the last 3 lines, the broken one skipped
    assert len(store.plans(100)) == 5
    store.plans_path.write_bytes(b"x" * 21_000_000)                   # over 20 MB: the file becomes *.old
    store.plan({"pid": "new"})
    assert store.plans_path.with_suffix(".old").exists() and store.plans() == [{"pid": "new"}]


# ------------------------------------------------------------------ the market filter
def others(t, changes, now=T0, synced=True):
    for i, ch in enumerate(changes):
        b = long_book(now, sym=f"S{i}USDT")
        old = 1.003 / (1 + ch)
        b.bars.appendleft([int((now - 1800) // 60), old + 0.0001, old - 0.0001])
        b.synced = synced
        t.books[f"S{i}USDT"] = b


def test_market_is_the_median_change_of_the_other_coins(tmp_path):
    t, broker, store = make(tmp_path, now=T0)
    others(t, [-0.02, -0.012, -0.011, -0.001, 0.003, 0.005])
    own = t.books["XUSDT"]
    own.bars.appendleft([int((T0 - 1800) // 60), 1.5, 1.5])          # XUSDT itself fell 33 %: not part of its median
    assert t.market("XUSDT", T0) == pytest.approx(-0.006, abs=1e-4)  # median of 6: the mean of the middle two
    del t.books["S0USDT"]
    t._mkt = (0.0, {})
    assert t.market("XUSDT", T0) == pytest.approx(-0.001, abs=1e-4)  # 5 others: the middle one
    del t.books["S1USDT"]
    t._mkt = (0.0, {})
    assert t.market("XUSDT", T0) is None                             # fewer than 5: no verdict


def test_market_ignores_blind_books_and_is_cached_for_10_s(tmp_path):
    t, broker, store = make(tmp_path, now=T0)
    others(t, [-0.02] * 6, synced=False)
    assert t.market("XUSDT", T0) is None
    for b in t.books.values():
        b.synced = True
    assert t.market("XUSDT", T0 + 5) is None                          # cached: refreshed every 10 s
    assert t.market("XUSDT", T0 + 11) == pytest.approx(-0.02, abs=1e-4)


def test_a_market_wide_fall_skips_the_long_and_a_calm_market_does_not(tmp_path):
    setup.P["mkt_max"] = 0.01
    t, broker, store = make(tmp_path, now=T0)
    others(t, [-0.02] * 6)
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], T0))
    assert not t.slots and next(iter(t.shadows.values())).act == "skip:market"
    assert next(iter(t.shadows.values())).plan.features["mkt"] == pytest.approx(-0.02, abs=1e-4)
    t, broker, store = make(tmp_path / "calm", now=T0)
    others(t, [-0.002, 0.001, 0.0, 0.003, -0.001, 0.002])
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], T0))
    assert "XUSDT" in t.slots and t.slots["XUSDT"].plan.features["mkt"] is not None
    t, broker, store = make(tmp_path / "few", now=T0)                  # no other coins: no filter
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], T0))
    assert "XUSDT" in t.slots and t.slots["XUSDT"].plan.features["mkt"] is None


# ------------------------------------------------------------------ risk
def test_day_loss_in_r_blocks_new_entries(tmp_path):
    t, broker, store = make(tmp_path)
    day = utc_day(NOW)
    RISK["day_loss_r"] = 6.0
    store.data["days"][day] = {"n": 12, "wins": 0, "pnl": -0.1, "gross": 0, "fees": 0, "r": -5.9}
    assert t.block(NOW) == ""                                         # -5.9 R: still free
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW))
    assert "XUSDT" in t.slots
    t.slots.clear()
    store.data["days"][day]["r"] = -6.0
    why = t.block(NOW)
    assert "R" in why and "-6.00R" in why and "USDT" not in why       # the pnl limit (-1 USDT) was not reached
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW))
    assert not t.slots and "-6.00R" in t.last_block
    store.data["days"] = {"1969-12-31": {"n": 12, "wins": 0, "pnl": -0.1, "gross": 0, "fees": 0, "r": -9.0}}
    assert t.block(NOW) == ""                                         # yesterday's R does not count
    store.data["days"] = {day: {"n": 12, "wins": 0, "pnl": -0.1, "gross": 0, "fees": 0, "r": -9.0}}
    RISK["day_loss_r"] = 0.0                                          # off
    assert t.block(NOW) == ""
    RISK["day_loss_r"] = 10.0
    assert t.block(NOW) == ""
    RISK["day_loss_r"] = 9.0
    assert "-9.00R" in t.block(NOW)


def losing(n, r=-0.5, ver=bounce.VERSION):
    return [{"ver": ver, "r": r} for _ in range(n)]


def test_live_guard_halts_on_a_bad_mean_r_only_when_live(tmp_path):
    RISK["guard_n"], RISK["guard_r"] = 30, -0.3
    t, broker, store = make(tmp_path)
    store.data["trades"] = losing(30)                                 # mean -0.5 R over 30 trades of this version
    t.live = False
    assert t.block(NOW) == ""                                         # paper: the guard is not applied
    t.live = True
    t._guard = None
    why = t.block(NOW)
    assert "halted" in why and "-0.50R over 30 trades" in why
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW))
    assert not t.slots and "halted" in t.halt                         # the trader halts


def test_live_guard_thresholds_and_versions(tmp_path):
    RISK["guard_n"], RISK["guard_r"] = 30, -0.3
    t, broker, store = make(tmp_path)
    t.live = True

    def verdict(trades):
        store.data["trades"] = trades
        t._guard = None
        return t.block(NOW)
    assert verdict(losing(29)) == ""                                  # one trade short
    assert verdict(losing(30, -0.29)) == ""                           # not below the limit
    assert verdict(losing(30, 0.4)) == ""
    assert verdict(losing(30, -0.5, ver="0.4.0")) == ""               # trades of an older version do not count
    assert "halted" in verdict(losing(10, -0.5, ver="0.4.0") + losing(30))
    assert verdict([{"ver": bounce.VERSION, "r": None}] * 40) == ""   # no R, no verdict
    RISK["guard_n"] = 0                                               # off
    assert verdict(losing(40)) == ""
    RISK["guard_n"], RISK["guard_r"] = 30, -1.0
    assert verdict(losing(40)) == ""                                  # -0.5 is above -1


def test_the_guard_verdict_is_cached_and_reset_when_a_trade_closes(tmp_path):
    RISK["guard_n"], RISK["guard_r"] = 30, -0.3
    t, broker, store = make(tmp_path)
    t.live = True
    store.data["trades"] = losing(29, -1.0)
    assert t.block(NOW) == "" and t._guard == ""                      # cached
    t.slots["XUSDT"] = Slot("XUSDT", setup.Plan("long", 1.0002, 0.9985, 1.0200, ("bid", 10000)), 9.0,
                            state="open", filled_qty=9.0, entry_px=1.0002, opened_at=time.time(), stop=0.9985)
    t.on_closed("XUSDT", 0.9980, -0.05, 0.01, "stop")                 # the 30th trade of v0.4.5: about -3 R
    assert t._guard is None
    assert "halted" in t.block(NOW)


# ------------------------------------------------------------------ SimBroker lag
def test_sim_cancel_lag_lets_a_fill_through(tmp_path):
    t, broker, store = make(tmp_path, lag=0.4)
    b = t.books["XUSDT"]

    async def go():
        await t._seek("XUSDT", b, NOW)
        s = t.slots["XUSDT"]
        b.densities[("bid", 10000)].gone = "pulled"
        await t._watch_entry(s, b, NOW + 1)                           # the cancel is on its way ...
        assert s.cancel_sent and s.state == "entry" and "XUSDT" in broker.o
        broker.on_trade("XUSDT", Trade(NOW + 1.2, 10001, 50.0, True))  # ... and the order still fills meanwhile
        await settle()
        assert s.state == "open"
        await asyncio.sleep(0.6)                                      # the late cancel finds a position: ignored
        assert t.slots["XUSDT"].state == "open" and broker.o["XUSDT"]["kind"] == "open"
    asyncio.run(go())
    assert stat(store, "cancelled") == 0 and stat(store, "filled") == 1


def test_sim_cancel_lag_takes_effect_later_when_nothing_fills(tmp_path):
    t, broker, store = make(tmp_path, lag=0.2)
    b = t.books["XUSDT"]

    async def go():
        await t._seek("XUSDT", b, NOW)
        b.densities[("bid", 10000)].gone = "pulled"
        await t._watch_entry(t.slots["XUSDT"], b, NOW + 1)
        assert "XUSDT" in t.slots                                     # not yet
        await asyncio.sleep(0.4)
        assert "XUSDT" not in t.slots and "XUSDT" not in broker.o
    asyncio.run(go())
    assert stat(store, "cancel_density_gone") == 1


def test_sim_without_lag_cancels_at_once_and_a_late_print_changes_nothing(tmp_path):
    t, broker, store = make(tmp_path, lag=0.0)
    b = t.books["XUSDT"]

    async def go():
        await t._seek("XUSDT", b, NOW)
        b.densities[("bid", 10000)].gone = "pulled"
        await t._watch_entry(t.slots["XUSDT"], b, NOW + 1)
        assert not t.slots and "XUSDT" not in broker.o
        broker.on_trade("XUSDT", Trade(NOW + 1.2, 10001, 50.0, True))
        await settle()
    asyncio.run(go())
    assert stat(store, "filled") == 0 and stat(store, "cancel_density_gone") == 1


def test_sim_new_entry_ignores_trades_older_than_the_lag(tmp_path):
    t, broker, store = make(tmp_path, lag=0.5)
    b = t.books["XUSDT"]

    async def go():
        await t._seek("XUSDT", b, NOW)
        s = t.slots["XUSDT"]
        assert broker.o["XUSDT"]["live_at"] == pytest.approx(NOW + 0.5)
        broker.on_trade("XUSDT", Trade(NOW + 0.3, 10001, 50.0, True))   # printed before the order reached the exchange
        await settle()
        assert s.state == "entry"
        broker.on_trade("XUSDT", Trade(NOW + 0.6, 10001, 50.0, True))
        await settle()
        assert s.state == "open"
    asyncio.run(go())
    t, broker, store = make(tmp_path / "nolag", lag=0.0)

    async def go0():
        await t._seek("XUSDT", t.books["XUSDT"], NOW)
        broker.on_trade("XUSDT", Trade(NOW + 0.3, 10001, 50.0, True))
        await settle()
        assert t.slots["XUSDT"].state == "open"
    asyncio.run(go0())


def test_the_runner_uses_a_sim_lag_of_0_3_s_unless_told_otherwise(tmp_path, monkeypatch):
    monkeypatch.delenv("SIM_LAG", raising=False)
    r = Runner(Store(tmp_path / "a.json", 100.0))
    assert isinstance(r.broker, SimBroker) and r.broker.lag == 0.3 and r.trader.live is False
    monkeypatch.setenv("SIM_LAG", "1.5")
    assert Runner(Store(tmp_path / "b.json", 100.0)).broker.lag == 1.5
    monkeypatch.setenv("SIM_LAG", "")
    assert Runner(Store(tmp_path / "c.json", 100.0)).broker.lag == 0.3
    assert SimBroker().lag == 0.0
    assert Runner(Store(tmp_path / "d.json", 100.0), mode="live").trader.live is True


def test_plans_json_is_valid_one_object_a_line(tmp_path):
    t, broker, store = make(tmp_path)
    sells(t.books["XUSDT"], NOW - 30, 120_000)
    asyncio.run(t._seek("XUSDT", t.books["XUSDT"], NOW))
    t._step_shadows(NOW + 200)
    lines = store.plans_path.read_text(encoding="utf-8").splitlines()
    assert len(lines) == 1 and json.loads(lines[0])["act"] == "skip:weak"
