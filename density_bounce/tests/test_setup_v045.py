"""v0.4.5 rules of setup.py and book.py: shadow plans, ATR gate, reality criterion, market filter, confirmation."""
import pytest

from bounce import book, setup
from bounce.book import SymbolBook, Trade
from bounce.setup import cancel_reason, plan_bounce
from tests.helpers import PIN_P, long_book, short_book

NOW = 1000.0


@pytest.fixture(autouse=True)
def fresh_params():
    saved, dens = dict(setup.P), dict(book.DENS)
    setup.P.update(PIN_P)                             # the values the tests depend on, not the defaults
    yield
    setup.P.clear()
    setup.P.update(saved)
    book.DENS.clear()
    book.DENS.update(dens)


def sells(b, ts, usd, tick=10030):
    """One aggressive SELL of `usd` USDT printed at `tick` (it hits a bid density's side)."""
    b.trades.append(Trade(ts, tick, usd / (tick * b.tick_size), True))


def buys(b, ts, usd, tick=9970):
    b.trades.append(Trade(ts, tick, usd / (tick * b.tick_size), False))


def test_scan_returns_the_tradable_plan_first_and_plan_bounce_is_its_first_item():
    b = long_book(NOW)
    p, ghost = setup.scan(b, NOW, set())
    assert p is not None and ghost is None and p.skip == "" and p.side == "long"
    assert plan_bounce(b, NOW, set()).entry == p.entry
    assert setup.scan(b, NOW, {("bid", 10000)}) == (None, None)       # a skipped density gives nothing at all


def test_no_atr_no_plan():
    setup.P["stop_atr"] = 2.0
    b = long_book(NOW, bars=False)                   # no 1m bars yet
    assert setup.scan(b, NOW, set()) == (None, None) and plan_bounce(b, NOW, set()) is None
    b = long_book(NOW, n_bars=13)                    # one bar short of atr_n = 14
    assert setup.scan(b, NOW, set()) == (None, None)
    b = long_book(NOW)
    b.bars.append([int(NOW // 60), 1.2, 0.8])        # the running minute is not complete: it does not count
    assert plan_bounce(b, NOW, set()) is not None and b.atr(NOW) == pytest.approx(0.0002)
    setup.P["stop_atr"] = 0.0                        # ATR off: the bare stop_buf is allowed again, as before
    assert plan_bounce(long_book(NOW, bars=False), NOW, set()).stop == pytest.approx(0.9985)


def test_young_density_is_a_shadow_plan_and_younger_than_shadow_age_is_nothing():
    setup.P["min_age"], setup.P["shadow_age"] = 60.0, 20.0
    p, ghost = setup.scan(long_book(NOW, age=30.0), NOW, set())      # 20 <= 30 < 60 s
    assert p is None and ghost is not None and ghost.skip == "young" and ghost.features["age_s"] == 30.0
    assert ghost.entry == pytest.approx(1.0002) and ghost.stop == pytest.approx(0.9985)   # a full plan, not traded
    assert setup.scan(long_book(NOW, age=10.0), NOW, set()) == (None, None)               # < shadow_age
    p, ghost = setup.scan(long_book(NOW, age=60.0), NOW, set())                           # exactly min_age: trades
    assert p is not None and ghost is None
    setup.P["min_age"] = 20.0                        # the old rule: 30 s is old enough
    assert plan_bounce(long_book(NOW, age=30.0), NOW, set()) is not None


def test_weak_density_is_a_shadow_plan_a_strong_one_trades_with_hold_s():
    setup.P["hold_min"] = 60.0
    b = long_book(NOW)                               # a 100k USDT bid density
    sells(b, NOW - 30, 120_000)                      # 2000 USDT/s of selling in the last minute: 50 s worth
    d = b.densities[("bid", 10000)]
    hold, rate = setup.hold_secs(b, d, NOW)
    assert rate == pytest.approx(2000.0, rel=1e-3) and hold == pytest.approx(50.0, rel=1e-3)
    p, ghost = setup.scan(b, NOW, set())
    assert p is None and ghost.skip == "weak" and ghost.features["hold_s"] == pytest.approx(50.0, abs=0.1)
    assert ghost.features["flow_s"] == 2000
    b = long_book(NOW)
    sells(b, NOW - 30, 30_000)                       # 500 USDT/s: 200 s worth
    p, ghost = setup.scan(b, NOW, set())
    assert p is not None and ghost is None and p.features["hold_s"] == pytest.approx(200.0, abs=0.5)
    assert p.features["flow_s"] == 500
    setup.P["hold_min"] = 0.0                        # the gate off: the weak density trades again
    b = long_book(NOW)
    sells(b, NOW - 30, 120_000)
    p = plan_bounce(b, NOW, set())
    assert p is not None and p.features["hold_s"] == pytest.approx(50.0, abs=0.1)


def test_hold_uses_the_faster_of_one_and_three_minutes_and_only_the_aggressor_that_hits_the_density():
    setup.P["hold_min"] = 60.0
    b = long_book(NOW)
    d = b.densities[("bid", 10000)]
    assert setup.hold_secs(b, d, NOW) == (None, 0.0)                 # no tape: the gate passes
    assert plan_bounce(b, NOW, set()) is not None
    sells(b, NOW - 120, 540_000)                     # nothing in the last minute, 3000 USDT/s over 3 min
    hold, rate = setup.hold_secs(b, d, NOW)
    assert rate == pytest.approx(3000.0, rel=1e-3) and hold == pytest.approx(100_000 / 3000, rel=1e-3)
    assert setup.scan(b, NOW, set())[1].skip == "weak"
    b = long_book(NOW)
    buys(b, NOW - 30, 5_000_000)                     # buyers lift the asks: no pressure on a bid density
    assert setup.hold_secs(b, b.densities[("bid", 10000)], NOW) == (None, 0.0)
    assert plan_bounce(b, NOW, set()) is not None
    s = short_book(NOW)                              # the mirror: an ask density feels the aggressive BUYS
    buys(s, NOW - 30, 120_000, tick=9970)
    assert setup.hold_secs(s, s.densities[("ask", 10000)], NOW)[0] == pytest.approx(50.0, rel=0.01)
    assert setup.scan(s, NOW, set())[1].skip == "weak"
    sells(s, NOW - 20, 9_000_000)                    # sellers do not hit an ask density
    assert setup.hold_secs(s, s.densities[("ask", 10000)], NOW)[0] == pytest.approx(50.0, rel=0.01)


def test_cancel_reason_density_weak():
    setup.P["hold_cancel"] = 30.0
    b = long_book(NOW)
    p = plan_bounce(b, NOW, set())
    sells(b, NOW - 5, 200_000)                       # 3333 USDT/s: the wall is exactly 30 s worth, not under it
    assert cancel_reason(b, p, NOW, NOW) == ""
    sells(b, NOW - 4, 150_000)                       # 5833 USDT/s: 17 s worth
    assert cancel_reason(b, p, NOW, NOW) == "density_weak"
    assert "density_weak" in setup.WALL_FAILED and "unconfirmed" in setup.WALL_FAILED
    assert "price_away" not in setup.WALL_FAILED and "entry_timeout" not in setup.WALL_FAILED
    setup.P["hold_cancel"] = 0.0
    assert cancel_reason(b, p, NOW, NOW) == ""


def test_market_filter_long_and_short():
    setup.P["mkt_max"] = 0.01
    b = long_book(NOW)
    p, ghost = setup.scan(b, NOW, set(), -0.012)     # the others fell 1.2 % in 30 min: every wall gets hit
    assert p is None and ghost.skip == "market" and ghost.features["mkt"] == -0.012
    assert setup.scan(b, NOW, set(), -0.01)[1].skip == "market"      # exactly mkt_max counts
    assert setup.scan(b, NOW, set(), -0.005)[0] is not None          # a small fall is fine
    assert setup.scan(b, NOW, set(), 0.03)[0] is not None            # a rising market is no reason against a long
    assert setup.scan(b, NOW, set(), None)[0] is not None            # unknown: no filter
    s = short_book(NOW)
    assert setup.scan(s, NOW, set(), 0.012)[1].skip == "market"      # a rising market against a short
    assert setup.scan(s, NOW, set(), -0.03)[0] is not None
    assert setup.scan(s, NOW, set(), 0.004)[0] is not None
    setup.P["mkt_max"] = 0.0                         # off
    assert setup.scan(b, NOW, set(), -0.05)[0] is not None


def test_several_soft_gates_are_joined_and_hard_gates_still_drop_the_plan():
    b = long_book(NOW, age=30.0)
    sells(b, NOW - 30, 120_000)
    p, ghost = setup.scan(b, NOW, set(), -0.02)
    assert p is None and ghost.skip == "young+weak+market"
    b = long_book(NOW, age=30.0)
    b.densities[("bid", 10000)].usd = 50_000.0       # half eaten: a hard gate, not a shadow plan
    assert setup.scan(b, NOW, set(), -0.02) == (None, None)


def test_tradable_plan_and_shadow_plan_are_reported_separately():
    # a second, nearer density 0.2 % below mid fails a soft gate (young); the far one trades
    b = long_book(NOW)
    k = 10010
    b.bids[k] = 100_000.0 / 1.001
    b.densities[("bid", k)] = book.Density("bid", k, 1.0010, NOW - 30, 100_000.0, 100_000.0, NOW)
    p, ghost = setup.scan(b, NOW, set())
    assert p is not None and p.dkey == ("bid", 10000)
    assert ghost is not None and ghost.dkey == ("bid", k) and ghost.skip == "young"


def test_touched_and_confirm_long():
    b = long_book(NOW)
    p = plan_bounce(b, NOW, set())                   # entry 1.0002 = tick 10002
    assert not setup.touched(b, p)                   # the best bid is 10029: price has not come down yet
    b.bids = {k: q for k, q in b.bids.items() if k <= 10002}
    assert setup.touched(b, p)                       # the best bid is the entry tick: an order there is first in line
    assert setup.confirm(b, p) == ("", 10002)
    b.asks = {10002: 100.0}                          # the best ask is 10002: a post-only bid there would cross
    assert setup.confirm(b, p) == ("", 10001)        # one tick below the ask
    b.asks = {10001: 100.0}                          # ... and 10000 is the density itself: wait
    assert setup.confirm(b, p) == ("", None)
    b.asks = {10003: 100.0}
    d = b.densities[("bid", 10000)]
    d.usd = 0.69 * d.peak_usd                        # under 70 % of its peak
    assert setup.confirm(b, p) == ("unconfirmed", None)
    d.usd = 0.7 * d.peak_usd
    assert setup.confirm(b, p) == ("", 10002)        # exactly intact
    d.gone = "pulled"
    assert setup.confirm(b, p) == ("unconfirmed", None)
    del b.densities[("bid", 10000)]
    assert setup.confirm(b, p) == ("unconfirmed", None)


def test_touched_and_confirm_short():
    b = short_book(NOW)
    p = plan_bounce(b, NOW, set())                   # entry 0.9998 = tick 9998
    assert p.side == "short" and not setup.touched(b, p)
    b.asks = {k: q for k, q in b.asks.items() if k >= 9998}
    assert setup.touched(b, p) and setup.confirm(b, p) == ("", 9998)
    b.bids = {9998: 100.0}                           # a post-only ask there would cross: one tick above the bid
    assert setup.confirm(b, p) == ("", 9999)
    b.bids = {9999: 100.0}
    assert setup.confirm(b, p) == ("", None)         # 10000 is the density
    b.densities[("ask", 10000)].usd = 10.0
    assert setup.confirm(b, p) == ("unconfirmed", None)


# book.py -----------------------------------------------------------------------
def test_flow_and_flow_rate_split_by_aggressor_and_window():
    b = SymbolBook("XUSDT", 0.0001)
    b.trades.append(Trade(900.0, 10000, 100.0, True))     # 100 x 1.0 sell, 100 s ago: 100 USDT
    b.trades.append(Trade(990.0, 20000, 10.0, True))      # 10 x 2.0 sell, 10 s ago: 20 USDT
    b.trades.append(Trade(995.0, 10000, 50.0, False))     # a buy: 50 USDT
    assert b.flow(1000.0, 60, "sell") == pytest.approx(20.0)
    assert b.flow(1000.0, 180, "sell") == pytest.approx(120.0)
    assert b.flow(1000.0, 180, "buy") == pytest.approx(50.0)
    assert b.flow(1000.0, 4, "buy") == 0.0
    assert b.flow_rate(1000.0, "sell") == pytest.approx(max(20 / 60, 120 / 180))   # the 3 min rate is the faster
    assert b.flow_rate(1000.0, "sell", 5, 8) == pytest.approx(0.0)                 # no sell in the last 8 s
    assert b.flow_rate(1000.0, "sell", 15, 180) == pytest.approx(max(20 / 15, 120 / 180))
    assert b.flow_rate(1000.0, "buy") == pytest.approx(50 / 60)
    assert SymbolBook("YUSDT", 0.0001).flow_rate(1000.0, "sell") == 0.0


def test_bar_ago_is_the_middle_of_that_minute_and_none_before_the_bars():
    b = SymbolBook("XUSDT", 0.0001)
    b.bars.extend([[10, 1.02, 1.00], [11, 1.05, 1.03], [12, 1.04, 1.04]])
    assert b.bar_ago(12 * 60 + 30, 30) == pytest.approx(1.04)        # minute 12
    assert b.bar_ago(12 * 60 + 30, 90) == pytest.approx(1.04)        # minute 11 -> (1.05 + 1.03) / 2
    assert b.bar_ago(12 * 60 + 30, 150) == pytest.approx(1.01)       # minute 10
    assert b.bar_ago(12 * 60 + 30, 3000) is None                     # before the oldest bar


def test_density_band_is_three_percent_the_baseline_two():
    def wall_book(price_k):
        b = SymbolBook("XUSDT", 0.0001)
        for i in range(1, 400):                       # a thin ladder, 100 units a level, 4 % deep
            b.bids[10000 - i] = 100.0
            b.asks[10000 + i] = 100.0
        b.bids[price_k] = 30_000.0                   # ~29k USDT: > 25k and > 8 x the median level
        b.last_ts = 1000.0
        return b
    assert book.DENS["band"] == 0.03 and book.DENS["base_band"] == 0.02
    b = wall_book(9750)                              # 2.5 % below mid: inside the 3 % band, outside the 2 % baseline
    b.scan_densities(1000.0)
    assert ("bid", 9750) in b.densities
    b = wall_book(9650)                              # 3.5 % below: not tracked
    b.scan_densities(1000.0)
    assert not b.densities
    book.DENS["band"] = 0.02                         # the old width would have missed the first one
    b = wall_book(9750)
    b.scan_densities(1000.0)
    assert not b.densities
