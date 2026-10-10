import pytest

from carry import paper
from carry.market import DAY_MS
from carry.state import fresh
from carry.strategy import FEE_SIDE

D0 = 1_791_590_400_000 - 1_791_590_400_000 % DAY_MS
D1 = D0 + DAY_MS


def plan(day, book, notional, prices, stopped=(), skipped=None):
    return {"day": day, "book": list(book), "notional": notional, "prices": prices, "score": {s: 0.0003 for s in book},
            "rehedge": [], "pair_stopped": list(stopped), "skipped": skipped or {}}


def test_open_mark_close_cycle():
    st = fresh(600.0)
    res = paper.apply(st, plan(D0, ["AUSDT"], {"AUSDT": 100.0}, {"AUSDT": (10.0, 10.0)}), {})
    assert st["capital"] == pytest.approx(600 - FEE_SIDE * 100)
    assert st["held"]["AUSDT"]["opened"] == D0 and res["events"][0][2]

    # next day: 3 settlements of 0.01 % on the short, spot +1 %, perp +1.5 % -> basis -0.5 % of N
    rates = {"AUSDT": [(D0 + h * 3_600_000, 0.0001) for h in (0, 8, 16)] + [(D1, 0.0001)]}
    marks = paper.mark(st["held"], {"AUSDT": (10.15, 10.1)}, rates, D1)
    assert marks["AUSDT"]["funding"] == pytest.approx(0.03)
    assert marks["AUSDT"]["basis"] == pytest.approx(100 * 0.01 - 100 * 0.015)
    before = st["capital"]
    paper.apply(st, plan(D1, [], {}, {"AUSDT": (10.15, 10.1)}), marks)
    assert st["held"] == {}
    assert st["capital"] == pytest.approx(before + 0.03 - 0.5 - FEE_SIDE * 100)
    closed = st["closed"][-1]
    assert closed["symbol"] == "AUSDT" and closed["pnl"] == pytest.approx(0.03 - 0.5 - 2 * FEE_SIDE * 100)
    assert st["stats"] == {"pairs_opened": 1, "pairs_closed": 1, "pair_stops": 0}


def test_pair_stop_sets_cooldown():
    st = fresh(600.0)
    paper.apply(st, plan(D0, ["AUSDT"], {"AUSDT": 100.0}, {"AUSDT": (10.0, 10.0)}), {})
    marks = paper.mark(st["held"], {"AUSDT": (10.3, 10.0)}, {}, D1)
    paper.apply(st, plan(D1, [], {}, {"AUSDT": (10.3, 10.0)}, stopped=["AUSDT"]), marks)
    assert st["cool"]["AUSDT"] == D1 + 7 * DAY_MS
    assert st["stats"]["pair_stops"] == 1


def test_resize_charges_only_the_difference():
    st = fresh(600.0)
    paper.apply(st, plan(D0, ["AUSDT"], {"AUSDT": 100.0}, {"AUSDT": (10.0, 10.0)}), {})
    c = st["capital"]
    marks = paper.mark(st["held"], {"AUSDT": (10.0, 10.0)}, {}, D1)
    paper.apply(st, plan(D1, ["AUSDT"], {"AUSDT": 50.0}, {"AUSDT": (10.0, 10.0)}), marks)
    assert st["capital"] == pytest.approx(c - FEE_SIDE * 50)
    assert st["held"]["AUSDT"]["n"] == 50.0 and st["held"]["AUSDT"]["perp0"] == 10.0
