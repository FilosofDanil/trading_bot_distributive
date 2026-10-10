import pandas as pd
import pytest

from carry import market, strategy
from carry.market import DAY_MS

DAY = 1_791_590_400_000 - 1_791_590_400_000 % DAY_MS


@pytest.fixture(autouse=True)
def no_network(monkeypatch):
    monkeypatch.setattr(market, "perpetuals", lambda: [])


def m(f8: float, perp=10.0, basis=0.0, young=False, range14=0.1):
    times = [DAY - 3 * DAY_MS + j * 8 * 3_600_000 for j in range(9)]
    f = pd.DataFrame({"fundingTime": times, "fundingRate": [f8] * 9, "f8": [f8] * 9})
    return {"perp_open": perp, "spot_open": perp / (1 + basis), "range14": range14, "f8": f,
            "spot_first": DAY - (2 if young else 30) * DAY_MS}


def test_opens_above_threshold_only():
    mk = {"AUSDT": m(0.0003), "BUSDT": m(0.00015)}
    p = strategy.decide(DAY, {}, 600.0, list(mk), market_data=mk)
    assert p["book"] == ["AUSDT"] and p["open"] == ["AUSDT"]
    # one coin of kmin 2: half the capital, legs N + margin 2N -> N = 300 / 3
    assert p["notional"]["AUSDT"] == pytest.approx(100.0)


def test_entry_filters():
    mk = {"AUSDT": m(0.0003, basis=0.03), "BUSDT": m(0.0003, young=True), "CUSDT": m(0.0003, range14=0.5)}
    p = strategy.decide(DAY, {}, 600.0, list(mk), market_data=mk)
    assert p["book"] == []
    assert set(p["skipped"]) == {"AUSDT", "BUSDT", "CUSDT"}


def test_keep_band_and_exit():
    mk = {"AUSDT": m(0.00012), "BUSDT": m(0.00005)}
    held = {s: {"n": 100.0, "ref": 10.0, "perp0": 10.0, "spot0": 10.0} for s in mk}
    p = strategy.decide(DAY, held, 600.0, list(mk), market_data=mk)
    assert p["book"] == ["AUSDT"] and p["close"] == ["BUSDT"]
    assert p["notional"]["AUSDT"] == 100.0           # target 100 is inside the 60 % band: unchanged


def test_pair_stop_and_cooldown():
    mk = {"AUSDT": m(0.0003, perp=10.3, basis=0.0)}   # spot +0 %, perp +3 % since entry: pair -3 %
    mk["AUSDT"]["spot_open"] = 10.0
    held = {"AUSDT": {"n": 100.0, "ref": 10.0, "perp0": 10.0, "spot0": 10.0}}
    p = strategy.decide(DAY, held, 600.0, ["AUSDT"], market_data=mk)
    assert p["pair_stopped"] == ["AUSDT"] and p["close"] == ["AUSDT"]
    p2 = strategy.decide(DAY, {}, 600.0, ["AUSDT"], market_data={"AUSDT": m(0.0003)}, cool={"AUSDT": DAY + DAY_MS})
    assert p2["book"] == []


def test_orders_equal_qty_and_minimum():
    plan = {"open": ["AUSDT"], "close": [], "resize": {}, "notional": {"AUSDT": 100.0}, "score": {"AUSDT": 0.0003},
            "prices": {"AUSDT": (10.0, 10.0)}, "held_n": {}}
    lot = lambda step, kind, key, mn: {"filters": [{"filterType": "LOT_SIZE", "stepSize": step},  # noqa: E731
                                                   {"filterType": kind, key: mn}]}
    fut = {"AUSDT": lot("0.1", "MIN_NOTIONAL", "notional", "5")}
    spot = {"AUSDT": lot("0.01", "NOTIONAL", "minNotional", "5")}
    (o,) = strategy.orders(plan, (fut, spot))
    assert o["spot"] == ("BUY", pytest.approx(10.0)) and o["perp"] == ("SELL", pytest.approx(10.0))
    assert o["ok"] and o["margin_to_futures"] == pytest.approx(200.0)
