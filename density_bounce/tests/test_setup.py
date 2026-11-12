import pytest

from bounce import book, setup
from bounce.book import SymbolBook
from bounce.feed import pick
from bounce.setup import Manage, Plan, cancel_reason, exit_reason, load_params, plan_bounce
from tests.helpers import PIN_P, long_book, make_book, short_book

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


def test_long_entry_in_front_stop_behind_take_at_middle_of_the_move():
    p = plan_bounce(long_book(NOW), NOW, set())
    assert p is not None and p.side == "long"
    assert p.entry == pytest.approx(1.0002)          # 0.02 % (2 ticks) in front of the density at 1.0000
    assert p.stop == pytest.approx(0.9985)           # 0.15 % behind it
    assert p.tp == pytest.approx(1.0200)             # middle of the move 1.0400 -> 1.0000
    assert p.dkey == ("bid", 10000)
    f = p.features
    assert f["move_start"] == pytest.approx(1.0400) and f["move"] == pytest.approx(0.0385, abs=1e-4)
    assert f["approach"] == pytest.approx(0.0108, abs=1e-4) and f["move_s"] == pytest.approx(1500)
    assert f["rr"] == pytest.approx((1.0200 - 1.0002) / (1.0002 - 0.9985), abs=0.01)
    assert p.tp1 == pytest.approx(1.0019) and p.tp_run == 0.0      # half at +1R (risk 0.0017), no runner
    assert f["atr"] == pytest.approx(0.0002 / 1.003, abs=1e-5) and f["trend"] is None   # tiny ATR, no hours
    assert f["accel"] == pytest.approx(2.54, abs=0.02) and f["counter"] is False        # 3 min speed vs the move's
    assert f["hold_s"] is None and f["mkt"] is None and p.skip == ""                    # no tape, no other coins


def test_short_is_the_mirror():
    p = plan_bounce(short_book(NOW), NOW, set())
    assert p is not None and p.side == "short"
    assert p.entry == pytest.approx(0.9998)
    assert p.stop == pytest.approx(1.0015)
    assert p.tp == pytest.approx(0.9800)             # middle of the move 0.9600 -> 1.0000
    assert p.tp1 == pytest.approx(0.9981) and p.tp_run == 0.0


def test_parts_when_switched_on():
    setup.P["tp1_r"], setup.P["tp1_frac"], setup.P["run_frac"] = 0.0, 0.25, 1.0
    p = plan_bounce(long_book(NOW), NOW, set())
    assert p is not None and p.tp == pytest.approx(1.0200)
    assert p.tp1 == pytest.approx(1.0100)            # 25 % of the move
    assert p.tp_run == pytest.approx(1.0400)         # the runner: back at the start of the move
    p = plan_bounce(short_book(NOW), NOW, set())
    assert p is not None and p.tp1 == pytest.approx(0.9900) and p.tp_run == pytest.approx(0.9600)


def test_a_bigger_move_has_a_longer_take():
    b = make_book([(1500, 1.0800), (180, 1.0140), (60, 1.0070), (0, 1.0030)], 1.0000, "bid", NOW)
    p = plan_bounce(b, NOW, set())
    assert p is not None and p.tp == pytest.approx(1.0400)


def test_coarse_tick_is_not_traded():
    # tick 0.0001 at a price of 0.40 = 2.5 bp: a valid setup otherwise
    b = make_book([(1500, 0.4200), (180, 0.4060), (60, 0.4028), (0, 0.4012)], 0.4000, "bid", NOW)
    assert plan_bounce(b, NOW, set()) is None
    setup.P["tick_max"] = 0.0004
    assert plan_bounce(b, NOW, set()) is not None


def test_thin_turnover_is_not_traded():
    assert plan_bounce(long_book(NOW, turnover=99_000.0), NOW, set()) is None
    assert plan_bounce(long_book(NOW, turnover=101_000.0), NOW, set()) is not None


def test_screener_drops_thin_coins():
    info = {s: {"tick": 0.0001} for s in ("AUSDT", "BUSDT")}
    tickers = [{"symbol": "AUSDT", "quoteVolume": "99000000", "lastPrice": "1", "priceChangePercent": "9"},
               {"symbol": "BUSDT", "quoteVolume": "101000000", "lastPrice": "1", "priceChangePercent": "1"}]
    assert pick(tickers, info, set(), set(), 16) == ["BUSDT"]


def test_slow_approach_is_not_traded():
    # a big move, but only 0.50 % of it in the last 3 min: the price drifts to the wall
    b = make_book([(1500, 1.0400), (180, 1.0080), (60, 1.0060), (0, 1.0030)], 1.0000, "bid", NOW)
    assert plan_bounce(b, NOW, set()) is None


def test_stalled_approach_is_not_traded():
    # 1.08 % in 3 min, but it has stopped: 0.05 % in the last 60 s
    b = make_book([(1500, 1.0400), (180, 1.0140), (60, 1.0035), (0, 1.0030)], 1.0000, "bid", NOW)
    assert plan_bounce(b, NOW, set()) is None


def test_no_history_for_the_approach_is_not_traded():
    b = make_book([(60, 1.0070), (0, 1.0030)], 1.0000, "bid", NOW)   # just subscribed: no mid from 3 min ago
    b.bars.append([int((NOW - 900) // 60), 1.04, 1.03])
    assert plan_bounce(b, NOW, set()) is None


def test_short_previous_move_is_not_traded():
    # sharp (0.89 %/3 min) but the whole move is only 1.19 %: noise, not a move
    b = make_book([(180, 1.0120), (60, 1.0070), (0, 1.0030)], 1.0000, "bid", NOW)
    assert plan_bounce(b, NOW, set()) is None


def test_a_move_older_than_the_window_does_not_count():
    # the 4 % high was 33 min ago; inside the 30 min window the move is only 1.96 %
    b = make_book([(2000, 1.0400), (1700, 1.0200), (180, 1.0140), (60, 1.0070), (0, 1.0030)], 1.0000, "bid", NOW)
    assert plan_bounce(b, NOW, set()) is None


def test_seeded_bars_measure_a_move_older_than_the_mid_samples():
    now = 100_000.0
    b = make_book([(180, 1.0140), (60, 1.0070), (0, 1.0030)], 1.0000, "bid", now)
    assert plan_bounce(b, now, set()) is None                        # 1.4 % seen: too short
    minute = int((now - 1500) // 60)
    b.seed_bars([[minute * 60_000, "1.03", "1.0400", "1.0300", "1.03"]])
    p = plan_bounce(b, now, set())
    assert p is not None and p.features["move_start"] == pytest.approx(1.0400)
    assert p.tp == pytest.approx(1.0200)


def test_seed_keeps_our_own_tape_and_goes_in_front_of_it():
    b = SymbolBook("XUSDT", 0.0001)
    b.on_trade({"T": 120_000, "p": "1.0000", "q": "1", "m": False})
    b.on_trade({"T": 121_000, "p": "1.0100", "q": "1", "m": False})
    b.seed_bars([[60_000, "1", "1.05", "0.95", "1"], [120_000, "1", "9.00", "0.10", "1"]])    # minute 2 is ours
    assert [list(x) for x in b.bars] == [[1, 1.05, 0.95], [2, pytest.approx(1.01), pytest.approx(1.0)]]


def test_young_or_eaten_density_is_not_traded():
    assert plan_bounce(long_book(NOW, age=5.0), NOW, set()) is None
    b = long_book(NOW)
    d = b.densities[("bid", 10000)]
    d.usd = 0.5 * d.peak_usd                         # half eaten already
    assert plan_bounce(b, NOW, set()) is None


def test_price_too_far_from_density_is_not_traded():
    b = make_book([(1500, 1.0400), (180, 1.0200), (60, 1.0110), (0, 1.0060)], 1.0000, "bid", NOW)    # 0.6 % away
    assert plan_bounce(b, NOW, set()) is None


def test_reward_risk_filter_and_skip_set():
    setup.P["min_rr"] = 12.0                         # reward 1.98 % / risk 0.17 % = 11.6
    assert plan_bounce(long_book(NOW), NOW, set()) is None
    setup.P["min_rr"] = 1.0
    assert plan_bounce(long_book(NOW), NOW, {("bid", 10000)}) is None


def test_exits_break_even_then_time():
    b = long_book(NOW)                               # mid 1.0030 = +1.65 R over the entry 1.0002
    p = Plan("long", 1.0002, 0.9985, 1.0050, ("bid", 10000))
    m = Manage(opened_at=NOW)
    assert exit_reason(b, p, 1.0002, m, NOW + 60) == ""              # the default: break-even only when tp1 fills
    setup.P["be_at_r"] = 0.7
    assert exit_reason(b, p, 1.0002, m, NOW + 60) == "be"            # the option: the bounce went, stop to break-even
    assert exit_reason(b, p, 1.0025, m, NOW + 60) == ""              # only +0.1 R from this entry
    m.be_done = True
    del b.densities[("bid", 10000)]                  # the wall is gone: no panic exit, the stop decides
    assert exit_reason(b, p, 1.0002, m, NOW + 60) == ""
    assert exit_reason(b, p, 1.0002, m, NOW + setup.P["max_hold"] + 1) == "time"
    setup.P["exit_on_gone"] = True
    assert exit_reason(b, p, 1.0002, m, NOW + 60) == ""              # already +1.6 R: let it run
    assert exit_reason(b, p, 1.0030, m, NOW + 60) == "density_gone"  # no progress yet


def bars(b, now, rng, n=14, mid=1.0100):
    """n complete 1m bars of the range `rng` before the current minute (highs far below the move's 1.04 start)."""
    cur = int(now // 60)
    for m in range(cur - n, cur):
        b.bars.append([m, mid + rng / 2, mid - rng / 2])


def hours(b, closes, last_hour=None):
    """Hour closes ending at hour `last_hour` (default: hours 0 .. len - 1)."""
    first = 0 if last_hour is None else last_hour - len(closes) + 1
    b.h1.extend([[first + i, c] for i, c in enumerate(closes)])


DOWN = [1.50 - 0.02 * i for i in range(25)]          # 1h closes falling: last < EMA20 -> trend -1
UP = [1.00 + 0.02 * i for i in range(25)]


def test_book_atr_trend_and_price_ago():
    b = long_book(NOW, bars=False)
    assert b.atr(NOW) is None                        # no bars yet
    bars(b, NOW, 0.004)
    b.bars.append([int(NOW // 60), 1.2, 0.8])        # the current, unfinished minute does not count
    assert b.atr(NOW) == pytest.approx(0.004)
    assert b.trend() is None
    hours(b, DOWN)
    assert b.trend() == -1
    b.h1.clear()
    hours(b, UP)
    assert b.trend() == 1
    assert b.price_ago(NOW, 60) == pytest.approx(1.0070)          # the mid samples
    assert b.price_ago(NOW, 600) == pytest.approx(1.0400)         # the newest sample at least 600 s old (1500 s)
    assert b.price_ago(NOW, 1800) is None                         # older than the samples and every bar
    b.bars.appendleft([int((NOW - 1800) // 60) - 1, 1.06, 1.04])
    assert b.price_ago(NOW, 1800) == pytest.approx(1.05)          # the middle of the bar of that minute


def test_seed_h1_keeps_our_own_hours():
    b = SymbolBook("XUSDT", 0.0001)
    b.on_trade({"T": 7_200_500, "p": "1.0100", "q": "1", "m": False})                         # hour 2
    b.seed_h1([[3_600_000, "1", "1", "1", "1.05"], [7_200_000, "1", "1", "1", "9.99"]])       # hour 2 is ours
    assert [list(x) for x in b.h1] == [[1, 1.05], [2, pytest.approx(1.01)]]


def test_stop_is_two_atr_behind_the_density():
    b = long_book(NOW, atr=0.006)                    # ATR 0.6 % > the 0.15 % floor
    p = plan_bounce(b, NOW, set())
    assert p is not None and p.stop == pytest.approx(1.0000 - 0.0120)
    assert p.tp1 == pytest.approx(1.0002 + (1.0002 - 0.9880))       # +1R from the entry
    assert p.features["atr"] == pytest.approx(0.006 / 1.003, abs=1e-5) and p.features["stop_atr"] == pytest.approx(2.03, abs=0.01)
    b2 = long_book(NOW, atr=0.0004)                  # a quiet coin: 2 x ATR < 0.15 %, the floor stays
    assert plan_bounce(b2, NOW, set()).stop == pytest.approx(0.9985)
    big = [(1500, 1.0800), (180, 1.0140), (60, 1.0070), (0, 1.0030)]       # an 8 % move: take 1.0400
    b3 = make_book(big, 1.0000, "bid", NOW, atr=0.010)   # 2 x ATR = 2 %: wide, but inside risk_max 4 %
    assert plan_bounce(b3, NOW, set()).stop == pytest.approx(0.9800)
    b4 = make_book(big, 1.0000, "bid", NOW, atr=0.025)   # 5 %: more than risk_max
    setup.P["min_rr"] = 0.0
    assert plan_bounce(b4, NOW, set()) is None
    setup.P["stop_atr"] = 0.0
    assert plan_bounce(b4, NOW, set()).stop == pytest.approx(0.9985)


T0 = 100_000.0                                       # the move starts at T0 - 1500 = 98 500: hour 26 closed before it
H0 = 26                                              # the last hour that closed before the move began


def counter_book(now, start):
    """long_book whose move starts at `start` 25 min ago: 1.20 = a 16 % move (3 min speed < the move's speed),
    1.05 = a 4.5 % move (the last 3 min are the fastest part)."""
    return make_book([(1500, start), (180, 1.0140), (60, 1.0070), (0, 1.0030)], 1.0000, "bid", now)


def test_against_the_1h_trend_only_after_a_sharp_approach():
    setup.P["counter_accel"] = 1.0
    # 1.20 -> 1.003 in 25 min = 16.4 %: the last 3 min (1.08 %) run at 0.55 x the speed of the move
    b = counter_book(T0, 1.20)
    hours(b, DOWN, H0)                               # a long into a falling 1h trend: counter
    p, ghost = setup.scan(b, T0, set())
    assert p is None and ghost is not None and ghost.skip == "counter_slow"     # not dropped: a shadow plan
    assert ghost.features["counter"] is True and ghost.features["accel"] == pytest.approx(0.55, abs=0.02)
    assert plan_bounce(b, T0, set()) is None
    b.h1.clear()
    hours(b, UP, H0)                                 # the same entry with the trend: fine
    p = plan_bounce(b, T0, set())
    assert p is not None and p.features["counter"] is False and p.features["accel"] == pytest.approx(0.55, abs=0.02)
    # 1.05: the move is 4.5 %, accel 2.0: a sharp approach, counter allowed
    b = counter_book(T0, 1.05)
    hours(b, DOWN, H0)
    p = plan_bounce(b, T0, set())
    assert p is not None and p.features["counter"] is True and p.features["trend"] == -1
    assert p.features["accel"] == pytest.approx(2.02, abs=0.02) and p.skip == ""
    setup.P["counter_accel"] = 0.0                   # the filter off
    b = counter_book(T0, 1.20)
    hours(b, DOWN, H0)
    assert plan_bounce(b, T0, set()) is not None
    setup.P["counter_accel"] = 2.5                   # a stricter bar turns the 4.5 % move into a shadow plan too
    b = counter_book(T0, 1.05)
    hours(b, DOWN, H0)
    assert setup.scan(b, T0, set())[1].skip == "counter_slow"


def test_counter_is_judged_by_the_trend_before_the_move_began():
    setup.P["counter_accel"] = 1.0
    b = counter_book(T0, 1.20)
    hours(b, DOWN, H0)                               # falling before the move: counter
    b.h1.append([H0 + 1, 3.0])                       # an hour that closes after the move began (28 * 3600 > 98 500)
    assert b.trend(20) == 1                          # measured now, the last close is far above the EMA
    assert b.trend(20, before=T0 - 1500) == -1       # measured before the move: the hour does not count
    assert setup.scan(b, T0, set())[1].skip == "counter_slow"
    assert b.trend(20, before=(H0 + 1) * 3600) == -1  # the close of hour H0 + 1 is at (H0 + 2) * 3600: not yet
    assert b.trend(20, before=(H0 + 2) * 3600) == 1
    assert b.trend(30, before=T0) is None            # fewer closes than the EMA length


def test_accel_is_none_without_history_or_when_the_move_is_not_toward_the_density():
    b = counter_book(T0, 1.20)
    assert setup.accel(b, T0, 1.0030, 1, 1.20, T0 - 1500) == pytest.approx(0.55, abs=0.02)
    assert setup.accel(b, T0, 1.0030, 1, 1.0020, T0 - 1500) is None            # start below mid: no move toward it
    assert setup.accel(b, T0, 1.0030, -1, 1.20, T0 - 1500) is None             # the other side: moving away
    young = make_book([(60, 1.0070), (0, 1.0030)], 1.0000, "bid", T0)
    assert setup.accel(young, T0, 1.0030, 1, 1.04, T0 - 1500) is None          # no mid from 3 min ago
    # a move shorter than approach_secs is judged over its own length: a straight drop is 1
    s = make_book([(120, 1.0300), (60, 1.0150), (0, 1.0000)], 0.99, "bid", T0)
    assert setup.accel(s, T0, 1.0000, 1, 1.0300, T0 - 120) == pytest.approx(1.0, abs=0.01)


def test_counter_entry_is_cancelled_when_the_approach_stalls():
    setup.P["cancel_after"] = 600.0
    setup.P["counter_accel"] = 1.0
    b = counter_book(T0, 1.05)
    hours(b, DOWN, H0)
    p = plan_bounce(b, T0, set())
    assert p is not None and p.features["counter"] and p.features["move_start"] == 1.05
    assert cancel_reason(b, p, T0, T0) == ""
    b.mids.append((T0 + 100, 1.0031))
    b.mids.append((T0 + 300, 1.0030))                # 3 min without progress toward the density
    assert cancel_reason(b, p, T0, T0 + 300) == "counter_stalled"
    p.features["counter"] = False                    # with the trend the stall is no reason
    assert cancel_reason(b, p, T0, T0 + 300) == ""


def test_a_density_being_eaten_is_not_traded_and_cancels_the_entry():
    b = long_book(NOW)
    d = b.densities[("bid", 10000)]
    p = plan_bounce(b, NOW, set())
    assert p is not None
    d.hist.extend([(NOW - 8, 100_000.0), (NOW - 4, 90_000.0)])
    d.usd = 78_000.0                                 # -22 % in 8 s, still >= 70 % of the peak
    assert d.drop(NOW, 10) == pytest.approx(0.22)
    assert plan_bounce(b, NOW, set()) is None
    assert cancel_reason(b, p, NOW - 1, NOW) == "density_eaten"
    assert d.drop(NOW + 20, 10) == 0.0               # the drop is older than 10 s
    setup.P["eat_cancel"] = 0.0
    assert plan_bounce(b, NOW, set()) is not None


def test_stop_depth_counts_the_book_beyond_the_stop():
    p = plan_bounce(long_book(NOW, depth=10.0), NOW, set())
    # stop 0.9985, band 0.3 % = 30 ticks: levels every 5 ticks behind the density at 9985 .. 9955 -> 7 x 10
    assert setup.stop_depth(long_book(NOW, depth=10.0), p) == pytest.approx(70.0)
    ps = plan_bounce(short_book(NOW, depth=10.0), NOW, set())
    assert setup.stop_depth(short_book(NOW, depth=10.0), ps) == pytest.approx(70.0)


def test_params_override():
    load_params('{"take_frac": 0.618, "exit_on_gone": true, "usd_min": 80000, "be_on_tp1": true, "atr_n": 10}')
    assert setup.P["be_on_tp1"] is True and setup.P["atr_n"] == 10
    assert setup.P["take_frac"] == 0.618 and setup.P["exit_on_gone"] is True
    assert book.DENS["usd_min"] == 80000.0                          # density keys go to book.DENS
    with pytest.raises(SystemExit):
        load_params('{"tak_frac": 0.5}')
