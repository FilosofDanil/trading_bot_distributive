import pytest

from bounce import book, setup
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
    assert p.tp == pytest.approx(1.0050)             # middle of the move 1.0100 -> 1.0000
    assert p.dkey == ("bid", 10000)
    f = p.features
    assert f["move_start"] == pytest.approx(1.0100) and f["move"] == pytest.approx(0.0099, abs=1e-4)
    assert f["rr"] == pytest.approx((1.0050 - 1.0002) / (1.0002 - 0.9985), abs=0.01)
    assert p.tp1 == pytest.approx(1.0019)            # +1 R (risk 0.0017)
    assert p.tp_run == pytest.approx(1.0100)         # the runner: back at the start of the move


def test_short_is_the_mirror():
    p = plan_bounce(short_book(NOW), NOW, set())
    assert p is not None and p.side == "short"
    assert p.entry == pytest.approx(0.9998)
    assert p.stop == pytest.approx(1.0015)
    assert p.tp == pytest.approx(0.9950)             # middle of the move 0.9900 -> 1.0000
    assert p.tp1 == pytest.approx(0.9981) and p.tp_run == pytest.approx(0.9900)


def test_first_take_at_most_half_way_to_the_take():
    setup.P["tp1_r"] = 5.0                           # +5 R is beyond the take: tp1 goes half way instead
    p = plan_bounce(long_book(NOW), NOW, set())
    assert p is not None and p.tp1 == pytest.approx(1.0026)


def test_coarse_tick_is_not_traded():
    # tick 0.0001 at a price of 0.40 = 2.5 bp: a valid setup otherwise
    b = make_book([(200, 0.4040), (60, 0.4028), (0, 0.4012)], 0.4000, "bid", NOW)
    assert plan_bounce(b, NOW, set()) is None
    setup.P["tick_max"] = 0.0004
    assert plan_bounce(b, NOW, set()) is not None


def test_slow_approach_is_not_traded():
    b = make_book([(200, 1.0100), (60, 1.0045), (0, 1.0030)], 1.0000, "bid", NOW)   # 0.15 % in 60 s
    assert plan_bounce(b, NOW, set()) is None


def test_short_previous_move_is_not_traded():
    # sharp (0.40 %/60 s) but the whole move is only 0.45 %: the take would not pay for the risk
    b = make_book([(60, 1.0045), (0, 1.0005)], 1.0000, "bid", NOW)
    assert plan_bounce(b, NOW, set()) is None


def test_young_or_eaten_density_is_not_traded():
    assert plan_bounce(long_book(NOW, age=5.0), NOW, set()) is None
    b = long_book(NOW)
    d = b.densities[("bid", 10000)]
    d.usd = 0.5 * d.peak_usd                         # half eaten already
    assert plan_bounce(b, NOW, set()) is None


def test_price_too_far_from_density_is_not_traded():
    b = make_book([(200, 1.0150), (60, 1.0110), (0, 1.0060)], 1.0000, "bid", NOW)    # 0.6 % away
    assert plan_bounce(b, NOW, set()) is None


def test_reward_risk_filter_and_skip_set():
    setup.P["min_rr"] = 3.0                          # reward 0.48 % / risk 0.17 % = 2.8
    assert plan_bounce(long_book(NOW), NOW, set()) is None
    setup.P["min_rr"] = 1.0
    assert plan_bounce(long_book(NOW), NOW, {("bid", 10000)}) is None


def test_exits_break_even_then_time():
    b = long_book(NOW)                               # mid 1.0030 = +1.65 R over the entry 1.0002
    p = Plan("long", 1.0002, 0.9985, 1.0050, ("bid", 10000))
    m = Manage(opened_at=NOW)
    assert exit_reason(b, p, 1.0002, m, NOW + 60) == "be"            # the bounce went: stop to break-even
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
