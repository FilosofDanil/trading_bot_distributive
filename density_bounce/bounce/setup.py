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
"""
from __future__ import annotations

import json
import math
import os
from dataclasses import dataclass, field

from .book import DENS, Density, SymbolBook

P = {
    "min_age": 20.0,          # s a density must live before we trust it
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

    @property
    def risk(self) -> float:
        return abs(self.entry - self.stop)


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


def accel(b: SymbolBook, now: float, mid: float, sgn: int) -> float | None:
    """Speed toward the density over the last approach_secs against the average speed over move_secs (None without
    that history). A window that as a whole did not move toward the density makes a live approach a spike out of
    nowhere (99, as sharp as it gets) and a stalled one 0."""
    m_app, p_move = b.mid_ago(now, P["approach_secs"]), b.price_ago(now, P["move_secs"])
    if m_app is None or p_move is None:
        return None
    app = (m_app - mid) / m_app * sgn
    v = (p_move - mid) / p_move * sgn
    if v <= 0:
        return 99.0 if app > 0 else 0.0
    return app / P["approach_secs"] / (v / P["move_secs"])


def _features(b: SymbolBook, d: Density, now: float, mid: float, spread: float) -> dict:
    turn = b.minute_turnover(now)
    return {"density_usd": round(d.usd), "peak_usd": round(d.peak_usd), "age_s": round(d.age(now), 1),
            "touches": d.touches, "dist": round(abs(mid - d.price) / mid, 5), "spread": round(spread, 5),
            "minute_usd": round(turn), "dens_vs_minute": round(d.peak_usd / turn, 2) if turn else None}


def plan_bounce(b: SymbolBook, now: float, skip: set) -> Plan | None:
    """The best (nearest) bounce entry on this book right now, or None."""
    mid, bb, ba = b.mid(), b.best_bid(), b.best_ask()
    if mid is None or bb is None or ba is None:
        return None
    spread = (ba - bb) * b.tick_size / mid
    if b.tick_size / mid > P["tick_max"] or spread > P["spread_max"] or b.minute_turnover(now) < P["minute_min"]:
        return None
    m_ago = b.mid_ago(now, P["impulse_secs"])
    if m_ago is None:
        return None
    atr = b.atr(now, P["atr_n"]) if P["stop_atr"] > 0 else None
    trend = b.trend(P["trend_ema"])
    best = None
    for d in b.alive():
        if (d.side, d.tick) in skip or d.age(now) < P["min_age"] or d.touches > P["max_touches"]:
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
        acc = accel(b, now, mid, sgn)
        counter = trend == -sgn
        if counter and P["counter_accel"] > 0 and acc is not None and acc < P["counter_accel"]:
            continue                                           # against the 1h trend, creeping in: no reversal sign
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
        if best is None or dist < best[0]:
            turn = b.minute_turnover(now)
            f = _features(b, d, now, mid, spread) | {
                "moved60": round(moved, 5), "approach": round(approach, 5), "move": round(move, 5),
                "move_start": start, "move_s": round(now - start_ts, 1), "risk": round(risk, 5),
                "reward": round(reward, 5), "rr": round(reward / risk, 2),
                "atr": round(atr / mid, 5) if atr else None,
                "stop_atr": round(abs(entry - stop) / atr, 2) if atr else None,
                "trend": trend, "counter": counter, "accel": round(acc, 2) if acc is not None else None,
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
            best = (dist, Plan("long" if long else "short", entry, stop, tp, (d.side, d.tick), f, tp1, tp_run))
    return best[1] if best else None


def cancel_reason(b: SymbolBook, plan: Plan, placed_at: float, now: float) -> str:
    """Why a working entry should be cancelled ('' = keep it)."""
    d = b.densities.get(plan.dkey)
    mid = b.mid()
    if d is None or d.gone:
        return "density_gone"
    if d.usd < P["cancel_shrink"] * d.peak_usd:
        return "density_shrank"
    if P["eat_cancel"] > 0 and d.drop(now, P["eat_secs"]) >= P["eat_cancel"]:
        return "density_eaten"
    if plan.features.get("counter") and P["counter_accel"] > 0 and mid:
        # against the 1h trend the entry needs the approach to stay sharp until the fill: the owner's "it crept in"
        a = accel(b, now, mid, 1 if plan.side == "long" else -1)
        if a is not None and a < P["counter_accel"]:
            return "counter_stalled"
    if mid and abs(mid - d.price) / mid > P["cancel_dist"]:
        return "price_away"
    if now - placed_at > P["cancel_after"]:
        return "entry_timeout"
    return ""


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
