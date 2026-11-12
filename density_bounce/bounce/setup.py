"""The density-bounce setup (pure functions of a SymbolBook): plan an entry, decide when to cancel or exit.

Long off a bid density (support), short off an ask density (resistance):

1. Density: a resting order >= 8x the median level, >= 25k USDT, >= half a minute of turnover (book.py DENS),
   alive >= 20 s (anti-spoof), still >= 70 % of its peak size, touched at most twice before. Coins whose tick
   is > 0.02 % of price or that trade < 100k USDT a minute are skipped (the screener also needs 100M USDT a day).
2. Sharp approach: mid moved >= 0.80 % toward the density in the last 3 min, is still moving (>= 0.15 % in the
   last 60 s, covering >= half of the distance it had 60 s ago) and is now within 0.35 % of the density.
3. The previous move: from the extreme of the last 30 min (the high before a drop onto a bid density, the low
   before a rally into an ask density) to the density; it must be >= 2.50 %.
4. The entry's quality (v0.4.0): pumping coins are fine, but a bounce against the 1h trend (price on the other side
   of the EMA20 of the hour closes) needs a sharp approach: the speed of the last 3 min >= the average speed of the
   30 min move (accel >= 1). A density that lost >= 20 % of its size in the last 10 s is being eaten: no entry.
5. Entry: post-only limit 0.02 % (>= 1 tick) in front of the density.
   Stop: max(0.15 %, 2 x ATR(14) of the 1m bars) behind the density (>= 2 ticks), so the stop is outside the noise
   of the move that brought price here; the trader sizes the position for a fixed USDT risk.
   Take: the middle of the previous move (50 % retracement from the density back toward the extreme), so a
   bigger move has a longer take. Skip unless reward >= 1.0 x risk and the risk is 0.10..4 % of price.
6. Exits: half the position at +1R, the rest at the take; the stop stays where it is (no break-even: the owner's
   note "break-even too early", v0.2.0), market close after 60 min. No early exit when the density disappears
   (the lab's forward test lost on exactly that exit, see README) unless P["exit_on_gone"] is set.
   The older options stay: tp1_r 0 with tp1_frac > 0 takes tp1_part at that fraction of the move, be_on_tp1 moves
   the stop to break-even (entry + fees) when tp1 fills, run_frac > 0 leaves the rest to run to that fraction of the
   move and the stop moves to tp1 when the take fills. Parts too small for the exchange are merged into the take.

The thresholds of 1-3 come from replaying 997 paper fills on 1m klines (README, "Why these thresholds"), those of 4-6
from the owner's notes on 73 live trades (docs/bounce_review_2026-11-10.md).

A working entry is cancelled when the density goes / shrinks below 50 % / loses 20 % in 10 s, an entry against the
1h trend loses its sharp approach (accel < 1 while waiting for the fill), price runs 0.8 % away, or after 3 min.
Every parameter is in P or book.DENS (both overridable with the PARAMS env var, a JSON object); every plan
carries its features.

v0.4.5 (the critique of 2026-11-12: 0 full takes in 30 trades of v0.3.0 + v0.4.0):
- Reality criterion: the density must be worth >= hold_min seconds of the aggressive flow that is hitting it NOW
  (the faster of the last 1 and 3 min of one-sided tape), so it can hold price for a minute. Until now the size
  was compared with the average turnover only when the density was first seen, an hour earlier in a calm market
  (ZEC: a 50k wall against 2.6M a minute). A working entry is cancelled when it falls under hold_cancel seconds.
- The density is tracked from 3 % away (book.DENS band) and must be min_age 60 s old, so its age is its own.
- No ATR (no bars yet): no trade. Until now the plan silently fell back to the 0.15 % stop.
- accel is the speed of the approach against the average speed of the move itself (extreme -> now), without the
  "99" special case; the 1h trend is the one BEFORE the move began (at the entry every bounce is counter-trend).
- Market filter: no entry when the other watched coins moved the same way (their median 30 min change >= mkt_max
  toward the density): in a market-wide dump every wall is hit at once (4 of 9 paper trades were one such dump).
- Confirmed entry: the order is armed first and placed confirm_secs after price reached the entry level, if the
  density is still intact (>= 70 % of its peak) and not being eaten.
- Soft gates (young, weak, counter_slow, market) do not drop the plan silently: `scan` returns it as a shadow plan
  with `skip` set, and the trader journals what the plain entry would have done (shadow.py).
"""
from __future__ import annotations

import json
import math
import os
from dataclasses import dataclass, field

from .book import DENS, Density, SymbolBook

P = {
    "min_age": 60.0,          # s a density must live before we trust it (20 until v0.4.5)
    "shadow_age": 20.0,       # younger than min_age but at least this old: not traded, journaled as a shadow plan
    "hold_min": 60.0,         # reality criterion: the density is worth >= this many seconds of the aggressive flow
                              # toward it (the faster of the last 1 and 3 min of one-sided tape); 0 = off
    "hold_cancel": 30.0,      # a working entry is cancelled when the density is worth < this many seconds (0 = off)
    "mkt_secs": 1800.0,       # market filter: the median change of the other watched coins over 30 min ...
    "mkt_max": 0.010,         # ... is >= 1 % toward the density (a market-wide move): no entry (0 = off)
    "confirm_secs": 15.0,     # place the entry this long after price reached the entry level, if the density
                              # is still intact (0 = place it at once, as until v0.4.5)
    "intact": 0.7,            # still >= 70 % of its peak size
    "max_touches": 2,         # 0..2 previous approaches
    "place_dist": 0.0035,     # mid within 0.35 % of the density
    "approach_secs": 180.0,   # sharp approach: in the last 3 min ...
    "approach_min": 0.0080,   # ... mid moved >= 0.80 % toward the density
    "impulse_secs": 60.0,     # still moving: in the last 60 s ...
    "impulse_min": 0.0015,    # ... mid moved >= 0.15 % toward the density ...
    "impulse_frac": 0.5,      # ... and covered >= half of the distance it had 60 s ago
    "move_secs": 1800.0,      # the previous move starts at the extreme of the last 30 min
    "move_min": 0.0250,       # and is >= 2.50 % long (start -> density)
    "take_frac": 0.5,         # take at 50 % of the move, measured from the density
    "minute_min": 100_000.0,  # skip coins trading < 100k USDT a minute (last 15 min of tape)
    "front": 0.0002,          # entry 0.02 % (>= 1 tick) in front of the density
    "stop_buf": 0.0015,       # stop at least 0.15 % (>= 2 ticks) behind the density ...
    "stop_atr": 2.0,          # ... and at least 2 x ATR of the 1m bars (0 = stop_buf only, as until v0.4.0)
    "atr_n": 14,
    "risk_min": 0.0010, "risk_max": 0.0400,   # wide is fine: the trader sizes for a fixed USDT risk
    "min_rr": 1.0,            # reward / risk at entry
    "tick_max": 0.0002,       # skip coins whose tick is > 0.02 % of price (the screener too)
    "spread_max": 0.0006,
    "trend_ema": 20,          # the 1h trend: last price vs the EMA of this many hour closes
    "counter_accel": 1.0,     # against the 1h trend only when the 3 min speed >= this x the 30 min speed (0 = off)
    "eat_secs": 10.0,         # skip / cancel when the density lost >= eat_cancel of its size in the last eat_secs
    "eat_cancel": 0.2,        # (0 = off)
    "stop_depth_k": 3.0,      # the book within stop_depth_band beyond the stop must hold >= 3 x the position (0 = off)
    "stop_depth_band": 0.003,
    "slip_max": 0.002,        # a stop filled > 0.2 % beyond its price slipped; slip_ban_n of them within
    "slip_ban_n": 2,          # slip_ban_s -> no trades on that symbol for slip_ban_s
    "slip_ban_s": 86400.0,
    "cancel_dist": 0.008, "cancel_after": 180.0, "cancel_shrink": 0.5,
    "max_hold": 3600.0,       # close at market after 60 min
    "exit_on_gone": False,    # close when the density disappears (off: the stop behind it is the exit)
    "be_at_r": 0.0,           # > 0: at +be_at_r R move the stop to entry + fees
    "be_on_tp1": False,       # move the stop to entry + fees when tp1 fills (v0.2.0; off: the stop stays)
    "be_off": 0.0006,         # break-even stop = entry + 0.06 % (maker entry + taker stop fees)
    "tp1_r": 1.0,             # > 0: first partial take at +1R from the entry ...
    "tp1_frac": 0.0,          # (with tp1_r 0 and this > 0: at this fraction of the move, measured from the density)
    "tp1_part": 0.5,          # ... for half of the position
    "tp2_part": 7 / 12,       # with a runner: the share taken at the take (the middle of the move)
    "run_frac": 0.0,          # > 0: the rest runs to this fraction of the move (1.0 = back at the move's start)
}


def load_params(raw: str | None = None) -> dict:
    """Apply PARAMS (JSON object) on top of P and book.DENS; unknown keys are an error so a typo never passes."""
    raw = os.environ.get("PARAMS", "") if raw is None else raw
    if not raw.strip():
        return P
    upd = json.loads(raw)
    bad = sorted(set(upd) - set(P) - set(DENS))
    if bad:
        raise SystemExit(f"PARAMS: unknown keys {bad}; known: {sorted(P) + sorted(DENS)}")
    for k, v in upd.items():
        d = P if k in P else DENS
        d[k] = type(d[k])(v)
    return P


@dataclass
class Plan:
    side: str                 # long | short
    entry: float
    stop: float
    tp: float
    dkey: tuple               # (side, tick) of the density
    features: dict = field(default_factory=dict)
    tp1: float = 0.0          # first partial take (0 = none)
    tp_run: float = 0.0       # the runner's take (0 = none)
    skip: str = ""            # soft gates that failed ("young+weak"): a shadow plan, journaled but not traded
    pid: str = ""             # journal id, set by the trader

    @property
    def risk(self) -> float:
        return abs(self.entry - self.stop)

    @property
    def sgn(self) -> int:
        return 1 if self.side == "long" else -1


def move_start(b: SymbolBook, now: float, secs: float, long: bool) -> tuple[float, float] | None:
    """(price, ts) of the extreme in the last `secs`: the max for a drop (long setup), the min for a rally.
    The mid samples cover the last minutes, the 1m bars (seeded when a symbol is added) the rest of the window."""
    best = None
    for ts, m in reversed(b.mids):
        if ts < now - secs:
            break
        if best is None or (m > best[0] if long else m < best[0]):
            best = (m, ts)
    for minute, hi, lo in reversed(b.bars):
        if minute * 60 < now - secs:
            break
        px = hi if long else lo
        if best is None or (px > best[0] if long else px < best[0]):
            best = (px, minute * 60.0)
    return best


def accel(b: SymbolBook, now: float, mid: float, sgn: int, start: float, start_ts: float) -> float | None:
    """Speed toward the density over the last approach_secs against the average speed of the whole move
    (`start` at `start_ts` -> now): > 1 = the move is accelerating into the density, < 1 = it is slowing down,
    <= 0 = the approach has stalled or turned. None without the mid history. A move shorter than approach_secs
    is compared over its own length, so a single straight drop is 1.
    (Until v0.4.5 the base was the net change over 30 min: with approach_min 0.8 % in 3 min the value could be
    < 1 only after a 30 min move of > 8 %, so the filter never fired, and a round trip scored a made-up 99.)"""
    dur = max(now - start_ts, 30.0)
    w = min(P["approach_secs"], dur)
    m_app = b.mid_ago(now, w)
    if m_app is None or start <= 0:
        return None
    tot = (start - mid) / start * sgn
    if tot <= 0:
        return None
    return (m_app - mid) / m_app * sgn / w / (tot / dur)


def hold_secs(b: SymbolBook, d: Density, now: float) -> tuple[float | None, float]:
    """(seconds the density can absorb the aggressive flow that is hitting its side now, that flow in USDT / s).
    None without tape."""
    rate = b.flow_rate(now, "sell" if d.side == "bid" else "buy", P["impulse_secs"], P["approach_secs"])
    return (d.usd / rate if rate > 0 else None), rate


def _features(b: SymbolBook, d: Density, now: float, mid: float, spread: float) -> dict:
    turn = b.minute_turnover(now)
    return {"density_usd": round(d.usd), "peak_usd": round(d.peak_usd), "age_s": round(d.age(now), 1),
            "touches": d.touches, "dist": round(abs(mid - d.price) / mid, 5), "spread": round(spread, 5),
            "minute_usd": round(turn), "dens_vs_minute": round(d.peak_usd / turn, 2) if turn else None}


def plan_bounce(b: SymbolBook, now: float, skip: set, mkt: float | None = None) -> Plan | None:
    """The best (nearest) bounce entry on this book right now that passes every gate, or None."""
    return scan(b, now, skip, mkt)[0]


def scan(b: SymbolBook, now: float, skip: set, mkt: float | None = None) -> tuple[Plan | None, Plan | None]:
    """(the nearest entry that passes every gate, the nearest one that fails only soft gates: a shadow plan with
    `skip` = "young" / "weak" / "counter_slow" / "market", joined by "+"). `mkt` = the median change of the other
    watched coins over mkt_secs (None = unknown: no market filter)."""
    mid, bb, ba = b.mid(), b.best_bid(), b.best_ask()
    if mid is None or bb is None or ba is None:
        return None, None
    spread = (ba - bb) * b.tick_size / mid
    if b.tick_size / mid > P["tick_max"] or spread > P["spread_max"] or b.minute_turnover(now) < P["minute_min"]:
        return None, None
    m_ago = b.mid_ago(now, P["impulse_secs"])
    if m_ago is None:
        return None, None
    atr = b.atr(now, P["atr_n"]) if P["stop_atr"] > 0 else None
    if P["stop_atr"] > 0 and not atr:
        return None, None                                      # no bars yet: the stop would be the bare stop_buf
    best: dict[bool, tuple[float, Plan]] = {}
    for d in b.alive():
        if (d.side, d.tick) in skip or d.touches > P["max_touches"]:
            continue
        if d.age(now) < min(P["min_age"], P["shadow_age"]):
            continue
        if d.usd < P["intact"] * d.peak_usd:
            continue
        if P["eat_cancel"] > 0 and d.drop(now, P["eat_secs"]) >= P["eat_cancel"]:
            continue                                           # being eaten right now: a breakout, not a wall
        long = d.side == "bid"
        sgn = 1 if long else -1
        dist = (mid - d.price) / mid * sgn
        if not (0 < dist <= P["place_dist"]):
            continue
        moved = (m_ago - mid) / m_ago * sgn                     # toward the density, as a fraction
        dist_ago = (m_ago - d.price) / m_ago * sgn
        if moved < P["impulse_min"] or dist_ago <= 0 or moved < P["impulse_frac"] * dist_ago:
            continue
        m_app = b.mid_ago(now, P["approach_secs"])
        if m_app is None:
            continue
        approach = (m_app - mid) / m_app * sgn
        if approach < P["approach_min"]:
            continue
        st = move_start(b, now, P["move_secs"], long)
        if st is None:
            continue
        start, start_ts = st
        move = (start - d.price) / start * sgn
        if move < P["move_min"]:
            continue
        acc = accel(b, now, mid, sgn, start, start_ts)
        trend = b.trend(P["trend_ema"], before=start_ts)       # the 1h trend before this move began
        counter = trend == -sgn
        hold, rate = hold_secs(b, d, now)
        mkt_to = None if mkt is None else -mkt * sgn           # the market's move toward the density
        why = []                                               # soft gates: journaled as a shadow plan
        if d.age(now) < P["min_age"]:
            why.append("young")
        if P["hold_min"] > 0 and hold is not None and hold < P["hold_min"]:
            why.append("weak")                                 # not worth a minute of the flow that is hitting it
        if counter and P["counter_accel"] > 0 and acc is not None and acc < P["counter_accel"]:
            why.append("counter_slow")                         # against the trend, slowing down: no reversal sign
        if P["mkt_max"] > 0 and mkt_to is not None and mkt_to >= P["mkt_max"]:
            why.append("market")                               # every coin is moving this way: walls do not hold
        front = max(1, math.ceil(P["front"] * d.price / b.tick_size))
        buf = max(2, math.ceil(P["stop_buf"] * d.price / b.tick_size))
        if atr:
            buf = max(buf, math.ceil(P["stop_atr"] * atr / b.tick_size - 1e-9))
        e_t = d.tick + sgn * front
        s_t = d.tick - sgn * buf
        if (long and e_t >= ba) or ((not long) and e_t <= bb):
            continue                                           # a post-only order would cross
        tp_raw = d.price + sgn * P["take_frac"] * abs(start - d.price)
        tp_t = math.floor(tp_raw / b.tick_size + 1e-9) if long else math.ceil(tp_raw / b.tick_size - 1e-9)
        entry, stop, tp = b.p(e_t), b.p(s_t), b.p(tp_t)
        risk = abs(entry - stop) / entry
        reward = (tp - entry) * sgn / entry
        if not (P["risk_min"] <= risk <= P["risk_max"]) or reward <= 0 or reward < P["min_rr"] * risk:
            continue
        ok = not why
        if ok not in best or dist < best[ok][0]:
            turn = b.minute_turnover(now)
            f = _features(b, d, now, mid, spread) | {
                "moved60": round(moved, 5), "approach": round(approach, 5), "move": round(move, 5),
                "move_start": start, "move_ts": round(start_ts, 1), "move_s": round(now - start_ts, 1),
                "risk": round(risk, 5), "reward": round(reward, 5), "rr": round(reward / risk, 2),
                "atr": round(atr / mid, 5) if atr else None,
                "stop_atr": round(abs(entry - stop) / atr, 2) if atr else None,
                "trend": trend, "counter": counter, "accel": round(acc, 2) if acc is not None else None,
                "hold_s": round(hold, 1) if hold is not None else None, "flow_s": round(rate),
                "mkt": round(mkt, 5) if mkt is not None else None,
                "vol_ratio": round(b.turnover(now, 180) / 3 / turn, 2) if turn else None,
                "ch24": b.ch24, "drop10": round(d.drop(now, P["eat_secs"]), 3)}
            # partial takes, rounded toward the entry (a take that is touched but not traded through stays unfilled)
            if P["tp1_r"] > 0:
                tp1_raw = entry + sgn * P["tp1_r"] * abs(entry - stop)
            else:
                tp1_raw = d.price + sgn * P["tp1_frac"] * abs(start - d.price)
            tp1_t = math.floor(tp1_raw / b.tick_size + 1e-9) if long else math.ceil(tp1_raw / b.tick_size - 1e-9)
            tp1 = b.p(tp1_t) if 0 < (tp1_t - e_t) * sgn < (tp_t - e_t) * sgn else 0.0
            run_raw = d.price + sgn * P["run_frac"] * abs(start - d.price)
            run_t = math.floor(run_raw / b.tick_size + 1e-9) if long else math.ceil(run_raw / b.tick_size - 1e-9)
            tp_run = b.p(run_t) if (run_t - tp_t) * sgn > 0 else 0.0
            best[ok] = (dist, Plan("long" if long else "short", entry, stop, tp, (d.side, d.tick), f, tp1, tp_run,
                                   "+".join(why)))
    return (best[True][1] if True in best else None), (best[False][1] if False in best else None)


WALL_FAILED = ("density_gone", "density_shrank", "density_eaten", "density_weak", "unconfirmed")


def cancel_reason(b: SymbolBook, plan: Plan, placed_at: float, now: float) -> str:
    """Why an armed or working entry should be cancelled ('' = keep it). The WALL_FAILED reasons say the density
    itself let us down: the trader does not try that density again for a while."""
    d = b.densities.get(plan.dkey)
    mid = b.mid()
    if d is None or d.gone:
        return "density_gone"
    if d.usd < P["cancel_shrink"] * d.peak_usd:
        return "density_shrank"
    if P["eat_cancel"] > 0 and d.drop(now, P["eat_secs"]) >= P["eat_cancel"]:
        return "density_eaten"
    if P["hold_cancel"] > 0:
        hold, _ = hold_secs(b, d, now)
        if hold is not None and hold < P["hold_cancel"]:
            return "density_weak"                              # the flow has grown: the wall is seconds of it now
    if plan.features.get("counter") and P["counter_accel"] > 0 and mid and plan.features.get("move_ts"):
        # against the 1h trend the entry needs the approach to stay sharp until the fill: the owner's "it crept in"
        a = accel(b, now, mid, plan.sgn, plan.features["move_start"], plan.features["move_ts"])
        if a is not None and a < P["counter_accel"]:
            return "counter_stalled"
    if mid and abs(mid - d.price) / mid > P["cancel_dist"]:
        return "price_away"
    if now - placed_at > P["cancel_after"]:
        return "entry_timeout"
    return ""


def touched(b: SymbolBook, plan: Plan) -> bool:
    """Price has reached the entry level: no bid is left above it (long) / no ask below it (short), so an order
    resting there would be first in line."""
    e_t = b.t(plan.entry)
    bb, ba = b.best_bid(), b.best_ask()
    if bb is None or ba is None:
        return False
    return bb <= e_t if plan.side == "long" else ba >= e_t


def confirm(b: SymbolBook, plan: Plan) -> tuple[str, int | None]:
    """After confirm_secs at the entry level: ('', tick to place the post-only entry at) when the density held,
    ('unconfirmed', None) when it did not (under intact of its peak or price is through it), ('', None) = wait
    (no room between the density and the opposite quote yet)."""
    d = b.densities.get(plan.dkey)
    bb, ba = b.best_bid(), b.best_ask()
    if d is None or d.gone or bb is None or ba is None or d.usd < P["intact"] * d.peak_usd:
        return "unconfirmed", None
    e_t, long = b.t(plan.entry), plan.side == "long"
    if long:
        t = min(e_t, ba - 1)                                   # post-only: below the best ask
        return ("", t) if t > d.tick else ("", None)
    t = max(e_t, bb + 1)
    return ("", t) if t < d.tick else ("", None)


def stop_depth(b: SymbolBook, plan: Plan) -> float:
    """Base qty resting from the stop to stop_depth_band beyond it on the side a market stop fills against (bids for
    a long), the density itself not counted: how much the stop can fill without sliding further."""
    long = plan.side == "long"
    s_t = b.t(plan.stop)
    band = max(1, round(P["stop_depth_band"] * plan.stop / b.tick_size))
    lo, hi = (s_t - band, s_t) if long else (s_t, s_t + band)
    return sum(q for k, q in (b.bids if long else b.asks).items() if lo <= k <= hi and k != plan.dkey[1])


@dataclass
class Manage:
    """Per-open-position memory for exit decisions."""
    opened_at: float
    be_done: bool = False


def exit_reason(b: SymbolBook, plan: Plan, entry_px: float, m: Manage, now: float) -> str:
    """'' = hold; 'be' = move the stop to break-even; anything else = close at market with that reason.
    Stop and take are resting orders (on the exchange in live mode); this only adds the time exit and options."""
    mid = b.mid()
    if mid is None:
        return ""
    if now - m.opened_at > P["max_hold"]:
        return "time"
    sgn = 1 if plan.side == "long" else -1
    r = abs(entry_px - plan.stop) or plan.risk
    prog = (mid - entry_px) * sgn / r
    if P["exit_on_gone"]:
        d = b.densities.get(plan.dkey)
        if (d is None or d.gone) and prog < 0.5:
            return "density_gone"
    if P["be_at_r"] > 0 and not m.be_done and prog >= P["be_at_r"]:
        return "be"
    return ""


def be_price(plan: Plan, entry_px: float, tick: float) -> float:
    """Break-even stop: entry + fees, but never closer to price than half the initial risk."""
    sgn = 1 if plan.side == "long" else -1
    r = abs(entry_px - plan.stop) or plan.risk
    raw = entry_px + sgn * min(P["be_off"] * entry_px, 0.5 * r)
    t = math.floor(raw / tick + 1e-9) if sgn > 0 else math.ceil(raw / tick - 1e-9)
    return round(t * tick, 12)
