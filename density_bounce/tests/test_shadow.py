"""bounce/shadow.py: what the plain entry would have done with a plan, followed on the tape."""
import pytest

from bounce import setup
from bounce.book import Trade
from bounce.setup import Plan
from bounce.shadow import STOP_SLIP, Shadow
from bounce.trader import FEE
from tests.helpers import PIN_P, long_book

NOW = 1000.0
ENTRY, STOP, TP, TP1 = 1.0002, 0.9985, 1.0200, 1.0019
RISK = ENTRY - STOP


@pytest.fixture(autouse=True)
def fresh_params():
    saved = dict(setup.P)
    setup.P.update(PIN_P)
    setup.P["cancel_after"], setup.P["max_hold"] = 180.0, 3600.0     # what these tests count on
    yield
    setup.P.clear()
    setup.P.update(saved)


def long_plan(**kw):
    return Plan("long", ENTRY, STOP, TP, ("bid", 10000), {}, kw.get("tp1", TP1))


def mk(plan=None, act="open", final=False):
    b = long_book(NOW)
    return b, Shadow("XUSDT", plan or long_plan(), NOW, act, final)


def tape(b, *prints):
    """prints = (seconds after NOW, tick, buyer_maker)."""
    for dt, tick, bm in prints:
        b.trades.append(Trade(NOW + dt, tick, 1.0, bm))


FILL = (1, 10001, True)             # a sell prints through our bid at 1.0002 (tick 10002)


def test_fill_then_stop_loses_one_r_plus_fees_and_the_slip():
    b, sh = mk()
    tape(b, FILL, (5, 9985, True))
    sh.step(b, NOW + 6, FEE)
    out = sh.out
    assert sh.state == "done" and out["hit"] == "stop" and out["fill_s"] == 1.0 and out["hold_s"] == 4.0
    exit_px = STOP * (1 - STOP_SLIP)
    want = (exit_px - ENTRY) / RISK - (FEE["maker"] + FEE["taker"]) * ENTRY / RISK
    assert out["r"] == pytest.approx(want, abs=0.002)
    assert -1.7 < out["r"] < -1.0                    # -1 R, minus the entry and stop fees and the slip
    assert out["mae_r"] == pytest.approx(-1.0, abs=0.01) and out["mfe_r"] == 0.0


def test_fill_then_take_with_the_half_at_one_r():
    b, sh = mk()
    tape(b, FILL, (5, 10020, False), (9, 10201, False))       # through tp1 (1.0019), then through the take
    sh.step(b, NOW + 10, FEE)
    out = sh.out
    assert out["hit"] == "take" and out["hold_s"] == 8.0
    gross = 0.5 * (TP1 - ENTRY) / RISK + 0.5 * (TP - ENTRY) / RISK
    fee_r = (FEE["maker"] + 0.5 * FEE["maker"] + 0.5 * FEE["maker"]) * ENTRY / RISK
    assert out["r"] == pytest.approx(gross - fee_r, abs=0.002) and out["r"] > 5
    assert out["mfe_r"] == pytest.approx((1.0201 - ENTRY) / RISK, abs=0.02)


def test_take_without_tp1_is_all_in_one_part():
    b, sh = mk(long_plan(tp1=0.0))
    tape(b, FILL, (9, 10201, False))
    sh.step(b, NOW + 10, FEE)
    want = (TP - ENTRY) / RISK - 2 * FEE["maker"] * ENTRY / RISK
    assert sh.out["hit"] == "take" and sh.out["r"] == pytest.approx(want, abs=0.002)


def test_tp1_then_stop_is_tp1_plus_stop():
    b, sh = mk()
    tape(b, FILL, (5, 10020, False), (8, 9985, True))
    sh.step(b, NOW + 10, FEE)
    exit_px = STOP * (1 - STOP_SLIP)
    gross = 0.5 * (TP1 - ENTRY) / RISK + 0.5 * (exit_px - ENTRY) / RISK
    fee_r = (FEE["maker"] + 0.5 * FEE["maker"] + 0.5 * FEE["taker"]) * ENTRY / RISK
    assert sh.out["hit"] == "tp1+stop" and sh.out["r"] == pytest.approx(gross - fee_r, abs=0.002)


def test_time_exit_closes_at_the_mid_after_max_hold():
    b, sh = mk()
    tape(b, FILL)
    sh.step(b, NOW + 100, FEE)
    assert sh.state == "open"
    sh.step(b, NOW + 1 + 3600 + 1, FEE)              # no new prints for an hour: the mid (1.0030) decides
    want = (1.0030 - ENTRY) / RISK - (FEE["maker"] + FEE["taker"]) * ENTRY / RISK
    assert sh.out["hit"] == "time" and sh.out["r"] == pytest.approx(want, abs=0.002)
    b, sh = mk()
    tape(b, FILL, (5, 10020, False))
    sh.step(b, NOW + 3700, FEE)
    assert sh.out["hit"] == "tp1+time"


def test_nofill_after_cancel_after():
    b, sh = mk()
    tape(b, (1, 10002, True), (50, 10005, True))     # a print AT our price does not fill us, nothing goes through
    sh.step(b, NOW + 60, FEE)
    assert sh.state == "wait"                        # still working
    sh.step(b, NOW + 181, FEE)                       # the plain entry is kept 180 s, then cancelled
    assert sh.state == "done" and sh.out == {"hit": "nofill", "r": None}


def test_a_print_through_the_price_after_cancel_after_is_too_late():
    b, sh = mk()
    tape(b, (200, 10001, True))
    sh.step(b, NOW + 201, FEE)
    assert sh.out["hit"] == "nofill"
    setup.P["cancel_after"] = 60.0
    b, sh = mk()
    tape(b, (70, 10001, True))
    sh.step(b, NOW + 71, FEE)
    assert sh.out["hit"] == "nofill"
    b, sh = mk()
    tape(b, (50, 10001, True))                       # inside the shorter window: a fill
    sh.step(b, NOW + 51, FEE)
    assert sh.state == "open"


def test_step_is_incremental_and_a_finished_shadow_stays_finished():
    b, sh = mk()
    tape(b, FILL)
    sh.step(b, NOW + 2, FEE)
    assert sh.state == "open" and sh.fill_ts == NOW + 1
    tape(b, (5, 9985, True))
    sh.step(b, NOW + 6, FEE)
    first = dict(sh.out)
    assert first["hit"] == "stop"
    tape(b, (7, 10201, False))
    sh.step(b, NOW + 8, FEE)
    assert sh.out == first
    sh.lost()
    assert sh.out == first                           # lost() does not overwrite a result


def test_short_plan_is_the_mirror():
    b = long_book(NOW)
    plan = Plan("short", 0.9998, 1.0015, 0.9800, ("ask", 10000), {}, 0.9981)
    sh = Shadow("XUSDT", plan, NOW, "open", False)
    tape(b, (1, 9999, False), (5, 9980, True), (9, 9799, True))    # a buy through our ask, tp1, the take
    sh.step(b, NOW + 10, FEE)
    risk = 1.0015 - 0.9998
    gross = 0.5 * (0.9998 - 0.9981) / risk + 0.5 * (0.9998 - 0.98) / risk
    fee_r = 2 * FEE["maker"] * 0.9998 / risk
    assert sh.out["hit"] == "take" and sh.out["r"] == pytest.approx(gross - fee_r, abs=0.002)
    sh2 = Shadow("XUSDT", plan, NOW, "open", False)
    b2 = long_book(NOW)
    tape(b2, (1, 9999, False), (5, 10015, False))
    sh2.step(b2, NOW + 6, FEE)
    assert sh2.out["hit"] == "stop" and sh2.out["r"] < -1.0


def test_lost_and_record():
    b, sh = mk(act="skip:weak", final=True)
    sh.plan.skip, sh.plan.pid = "weak", "XUSDT-1"
    sh.plan.features = {"hold_s": 12.0}
    sh.lost()
    assert sh.state == "done" and sh.out == {"hit": "lost", "r": None}
    rec = sh.record("0.4.5")
    assert set(rec) == {"pid", "ver", "ts", "sym", "side", "entry", "stop", "tp", "tp1", "skip", "act", "shadow",
                        "features"}
    assert rec["pid"] == "XUSDT-1" and rec["ver"] == "0.4.5" and rec["act"] == "skip:weak" and rec["skip"] == "weak"
    assert rec["side"] == "long" and rec["entry"] == ENTRY and rec["tp1"] == TP1 and rec["ts"] == NOW
    assert rec["shadow"] == {"hit": "lost", "r": None} and rec["features"] == {"hold_s": 12.0}
