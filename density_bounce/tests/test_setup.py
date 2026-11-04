import pytest

from bounce import book, setup
from bounce.book import SymbolBook
from bounce.feed import pick
from bounce.setup import Manage, Plan, exit_reason, load_params, plan_bounce
from tests.helpers import long_book, make_book, short_book

NOW = 1000.0


@pytest.fixture(autouse=True)
def fresh_params():
    saved, dens = dict(setup.P), dict(book.DENS)
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
    assert p.tp1 == 0.0 and p.tp_run == 0.0          # one take by default


def test_short_is_the_mirror():
    p = plan_bounce(short_book(NOW), NOW, set())
    assert p is not None and p.side == "short"
    assert p.entry == pytest.approx(0.9998)
    assert p.stop == pytest.approx(1.0015)
    assert p.tp == pytest.approx(0.9800)             # middle of the move 0.9600 -> 1.0000
    assert p.tp1 == 0.0 and p.tp_run == 0.0


def test_parts_when_switched_on():
    setup.P["tp1_frac"], setup.P["run_frac"] = 0.25, 1.0
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


def test_params_override():
    load_params('{"take_frac": 0.618, "exit_on_gone": true, "usd_min": 80000}')
    assert setup.P["take_frac"] == 0.618 and setup.P["exit_on_gone"] is True
    assert book.DENS["usd_min"] == 80000.0                          # density keys go to book.DENS
    with pytest.raises(SystemExit):
        load_params('{"tak_frac": 0.5}')
