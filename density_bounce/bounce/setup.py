"""The density-bounce setup (pure functions of a SymbolBook): plan an entry, decide when to cancel or exit.

Long off a bid density (support), short off an ask density (resistance):

1. Density: a resting order >= 8x the median level, >= 50k USDT, >= half a minute of turnover (book.py DENS),
   alive >= 20 s (anti-spoof), still >= 70 % of its peak size, touched at most twice before. Coins whose tick
   is > 0.02 % of price are skipped (a stop a few ticks wide slips by whole ticks).
2. Sharp approach: mid moved >= 0.30 % toward the density in the last 60 s and covered >= half of the distance
   it had 60 s ago; it is now within 0.35 % of the density.
3. The previous move: from the extreme of the last 5 min (the high before a drop onto a bid density, the low
   before a rally into an ask density) to the density; it must be >= 0.50 %.
4. Entry: post-only limit 0.02 % (>= 1 tick) in front of the density.
   Stop: 0.15 % (>= 2 ticks) behind the density.
   Take: the middle of the previous move (50 % retracement from the density back toward the extreme).
   Skip unless reward >= 1.0 x risk and the risk is 0.10..0.80 % of price.
5. Exits, taken in parts: 1/2 at +1 R (tp1; at most half way to the take), 1/3 at the take (tp2), and the last
   1/6 runs toward the start of the move (run). When the bounce goes (+0.7 R) or tp1 fills the stop moves to
   break-even (entry + fees); when tp2 fills it moves to tp1. Parts too small for the exchange are merged into
   tp2 (a small position has one take, as before). A market close after 15 min. No early exit when the density
   disappears (the lab's forward test lost on exactly that exit, see README) unless P["exit_on_gone"] is set.

A working entry is cancelled when the density goes / shrinks below 50 %, price runs 0.8 % away, or after 3 min.
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
    "impulse_secs": 60.0,     # sharp approach: in the last 60 s ...
    "impulse_min": 0.0030,    # ... mid moved >= 0.30 % toward the density ...
    "impulse_frac": 0.5,      # ... and covered >= half of the distance it had 60 s ago
    "move_secs": 300.0,       # the previous move starts at the extreme of the last 5 min
    "move_min": 0.0050,       # and is >= 0.50 % long (start -> density)
    "take_frac": 0.5,         # take at 50 % of the move, measured from the density
    "front": 0.0002,          # entry 0.02 % (>= 1 tick) in front of the density
    "stop_buf": 0.0015,       # stop 0.15 % (>= 2 ticks) behind the density
    "risk_min": 0.0010, "risk_max": 0.0080,
    "min_rr": 1.0,            # reward / risk at entry
    "tick_max": 0.0002,       # skip coins whose tick is > 0.02 % of price (the screener too)
    "spread_max": 0.0006,
    "cancel_dist": 0.008, "cancel_after": 180.0, "cancel_shrink": 0.5,
    "max_hold": 900.0,        # close at market after 15 min
    "exit_on_gone": False,    # close when the density disappears (off: the stop behind it is the exit)
    "be_at_r": 0.7,           # > 0: at +be_at_r R move the stop to entry + fees (0 = only when tp1 fills)
    "be_off": 0.0006,         # break-even stop = entry + 0.06 % (maker entry + taker stop fees)
    "tp1_r": 1.0,             # first partial take at +1 R (at most half way to the take) ...
    "tp1_part": 0.5,          # ... for half of the position
    "tp2_part": 1 / 3,        # a third at the take (the middle of the move); the rest (1/6) runs
    "run_frac": 1.0,          # the runner's take: 100 % of the move = back at the move's start
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
    """(price, ts) of the extreme mid in the last `secs`: the max for a drop (long setup), the min for a rally."""
    best = None
    for ts, m in reversed(b.mids):
        if ts < now - secs:
            break
        if best is None or (m > best[0] if long else m < best[0]):
            best = (m, ts)
    return best


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
    if b.tick_size / mid > P["tick_max"] or spread > P["spread_max"]:
        return None
    m_ago = b.mid_ago(now, P["impulse_secs"])
    if m_ago is None:
        return None
    best = None
    for d in b.alive():
        if (d.side, d.tick) in skip or d.age(now) < P["min_age"] or d.touches > P["max_touches"]:
            continue
        if d.usd < P["intact"] * d.peak_usd:
            continue
        long = d.side == "bid"
        sgn = 1 if long else -1
        dist = (mid - d.price) / mid * sgn
        if not (0 < dist <= P["place_dist"]):
            continue
        moved = (m_ago - mid) / m_ago * sgn                     # toward the density, as a fraction
        dist_ago = (m_ago - d.price) / m_ago * sgn
        if moved < P["impulse_min"] or dist_ago <= 0 or moved < P["impulse_frac"] * dist_ago:
            continue
        st = move_start(b, now, P["move_secs"], long)
        if st is None:
            continue
        start, start_ts = st
        move = (start - d.price) / start * sgn
        if move < P["move_min"]:
            continue
        front = max(1, math.ceil(P["front"] * d.price / b.tick_size))
        buf = max(2, math.ceil(P["stop_buf"] * d.price / b.tick_size))
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
            f = _features(b, d, now, mid, spread) | {
                "moved60": round(moved, 5), "move": round(move, 5), "move_start": start,
                "move_s": round(now - start_ts, 1), "risk": round(risk, 5), "reward": round(reward, 5),
                "rr": round(reward / risk, 2)}
            # partial takes, rounded toward the entry (a take that is touched but not traded through stays unfilled)
            tp1_raw = entry + sgn * min(P["tp1_r"] * abs(entry - stop), 0.5 * abs(tp - entry))
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
    if mid and abs(mid - d.price) / mid > P["cancel_dist"]:
        return "price_away"
    if now - placed_at > P["cancel_after"]:
        return "entry_timeout"
    return ""


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
