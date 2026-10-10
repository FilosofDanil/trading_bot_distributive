"""The carry decision and the orders it implies, ported from Strategy_tester/lab/carry_live.py.

`decide` and `orders` are kept line-for-line with the lab (the lab's `check` proved decide() == run_carry's
book 92/92 days on run2); only the data access goes through carry.market. Rules (safe carry, "buffer_calm"):
  score        mean funding of the last 3 days, scaled to 8 h; open >= 0.02 %, keep while >= 0.01 %, top 8
  filters      a NEW pair only while |perp / spot - 1| <= 2 % and no daily range > 35 % in the last 14 days
  size         both legs N each, perp margin 2N (isolated, 1x): a coin ties up 3N; capital split over max(coins, 2);
               a held pair is resized only when its target differs by > 60 %; never more than the capital
  margin       a held pair whose perp rose >= +100 % since entry / last re-hedge: re-hedge (alert)
  pair stop    close a pair whose result since entry, from the basis change alone, is <= -2 % of the leg;
               no re-entry for 7 days
  delisting    leave a pair a week before its perp delists (deliveryDate in exchangeInfo)
"""
from __future__ import annotations

import math

import pandas as pd

from . import market
from .market import DAY_MS

P = {"k": 8, "kmin": 2, "min_f8": 0.0002, "exit_f8": 0.0001, "lookback": 3,
     "buffer": 2.0, "calm": 0.35, "band": 0.6, "margin": 0.5, "delist_days": 7, "pair_stop": 0.02}
SPOT_MIN_DAYS = 7          # spot must have traded at least this long before a hedge is opened
MAX_ENTRY_BASIS = 0.02     # no new hedge while |perp / spot - 1| > 2 %
FEE_SIDE = 0.0010 + 0.0005 + 2 * 0.0002   # one entry (or exit) of the hedged pair, per unit of N (lab/carry.py)


def funding_8h(sym: str, start: int, end: int) -> pd.DataFrame:
    end_day = end - end % DAY_MS + DAY_MS                  # day-aligned window (live windows end every hour)
    f = market.funding(sym, start - 10 * DAY_MS, end_day)
    if f.empty:
        return f
    f = f[(f["fundingTime"] >= start - 10 * DAY_MS) & (f["fundingTime"] < end)].sort_values("fundingTime")
    gap = (f["fundingTime"].diff().rolling(3, min_periods=1).median().bfill() / 3_600_000).clip(1, 8).fillna(8)
    f = f.assign(f8=f["fundingRate"] * 8 / gap)
    return f.reset_index(drop=True)


def _perp_daily(sym: str, day: int, days: int):
    return market.fetch_klines(sym, "1d", day - days * DAY_MS, day + DAY_MS)


def _spot_daily(sym: str, day: int, days: int):
    return market.spot_daily_open(sym, day - days * DAY_MS, day + DAY_MS)


# ---------------------------------------------------------------- the decision (mirrors run_carry, manage=safe)
def decide(day: int, held: dict[str, dict], equity: float, cands: list[str],
           market_data: dict | None = None, cool: dict[str, int] | None = None) -> dict:
    """held: {sym: {"n": notional per leg, "ref": perp price at entry / last re-hedge, "perp0", "spot0"}}.
    market_data (optional, for tests): {sym: {"perp_open", "spot_open", "spot_first", "range14", "f8": DataFrame}}."""
    scores, info, skipped = [], {}, {}
    for s in sorted(set(cands) | set(held)):
        m = (market_data or {}).get(s)
        if m is None:
            f = funding_8h(s, day - (P["lookback"] + 1) * DAY_MS, day)
            pk = _perp_daily(s, day, 15)
            sp = _spot_daily(s, day, SPOT_MIN_DAYS + 1)
            # today's daily bar is still open (fetch_klines keeps closed bars): its open = yesterday's close
            yday = pk[pk["open_time"] == day - DAY_MS] if not pk.empty else pk
            if pk.empty or (day not in set(pk["open_time"]) and yday.empty) or day not in sp:
                skipped[s] = "no price for today"
                continue
            today_open = float(pk[pk["open_time"] == day].iloc[0]["open"]) if day in set(pk["open_time"]) \
                else float(yday.iloc[0]["close"])
            past = pk[(pk["open_time"] < day) & (pk["open_time"] >= day - 14 * DAY_MS)]
            m = {"perp_open": today_open, "spot_open": sp[day], "spot_first": min(sp),
                 "range14": float((past["high"] / past["low"] - 1).max()) if len(past) else 0.0, "f8": f}
        info[s] = m
        f = m["f8"]
        w = f[(f["fundingTime"] < day) & (f["fundingTime"] >= day - P["lookback"] * DAY_MS)] if len(f) else f
        if len(w) >= 2:
            scores.append((float(w["f8"].mean()), s))
    scores.sort(reverse=True)
    score_of = {s: sc for sc, s in scores}
    keep = [s for sc, s in scores if s in held and sc >= P["exit_f8"]]
    new = [s for sc, s in scores if s not in held and sc >= P["min_f8"] and (cool or {}).get(s, 0) <= day]
    delist = {x["symbol"]: int(x.get("deliveryDate", 1 << 62)) for x in market.perpetuals()}   # set when announced
    book = []
    for s in (keep + new)[:P["k"]]:
        m = info[s]
        if market_data is None and day + P["delist_days"] * DAY_MS >= delist.get(s, 1 << 62):
            skipped[s] = "perp delisting announced (deliveryDate within 7 days)"
            continue
        if m["spot_first"] > day - SPOT_MIN_DAYS * DAY_MS:
            skipped[s] = "spot younger than 7 days"
            continue
        if s not in held and abs(m["perp_open"] / m["spot_open"] - 1) > MAX_ENTRY_BASIS:
            skipped[s] = f"basis {m['perp_open'] / m['spot_open'] - 1:+.2%} > 2 %"
            continue
        if s not in held and m["range14"] > P["calm"]:
            skipped[s] = f"daily range {m['range14']:.0%} in 14 d > 35 %"
            continue
        book.append(s)
    tie = 1 + P["buffer"]
    ns: dict[str, float] = {}
    if book:
        total = equity * len(book) / max(len(book), P["kmin"])
        ns = {s: total / len(book) / tie for s in book}
        for s in book:
            if s in held and abs(ns[s] - held[s]["n"]) <= P["band"] * held[s]["n"]:
                ns[s] = held[s]["n"]
        excess = tie * sum(ns.values()) - equity
        if excess > 1e-9:
            fresh = [s for s in book if s not in held]
            room = tie * sum(ns[s] for s in fresh)
            cut = min(excess, room)
            for s in fresh:
                ns[s] *= 1 - cut / room if room > 0 else 1
            excess -= cut
            if excess > 1e-9:
                old = tie * sum(ns[s] for s in book if s in held)
                for s in book:
                    if s in held:
                        ns[s] *= 1 - excess / old
    rehedge = [s for s in book if s in held and info[s]["perp_open"] / held[s]["ref"] - 1 >= P["margin"] * P["buffer"]]
    stopped = []
    for s in list(book):
        h = held.get(s)
        if h and h.get("perp0") and h.get("spot0"):
            pair = (info[s]["spot_open"] / h["spot0"] - 1) - (info[s]["perp_open"] / h["perp0"] - 1)
            if pair <= -P["pair_stop"]:
                stopped.append(s)
                book.remove(s)
                ns.pop(s, None)
    return {"day": day, "equity": equity, "book": book, "notional": ns, "score": {s: score_of.get(s) for s in book},
            "open": [s for s in book if s not in held], "close": [s for s in held if s not in book],
            "resize": {s: (held[s]["n"], ns[s]) for s in book if s in held and abs(ns[s] - held[s]["n"]) > 1e-9},
            "rehedge": rehedge, "pair_stopped": stopped, "skipped": skipped, "top": [(s, sc) for sc, s in scores[:5]],
            "prices": {s: (info[s]["perp_open"], info[s]["spot_open"]) for s in info if s in book or s in held}}


# ---------------------------------------------------------------- orders it WOULD place
def _filters() -> tuple[dict, dict]:
    fut = {s["symbol"]: s for s in market.perpetuals()}
    spot = {s["symbol"]: s for s in market.spot_info()["symbols"]}
    return fut, spot


def _step_min(sym_info: dict, spot: bool) -> tuple[float, float]:
    step, mn = 0.0, 5.0
    for f in sym_info.get("filters", []):
        if f["filterType"] == "LOT_SIZE":
            step = float(f["stepSize"])
        if spot and f["filterType"] in ("NOTIONAL", "MIN_NOTIONAL"):
            mn = float(f.get("minNotional", mn))
        if not spot and f["filterType"] == "MIN_NOTIONAL":
            mn = float(f.get("notional", mn))
    return step, mn


def _qty(notional: float, price: float, step: float) -> float:
    q = notional / price
    return math.floor(q / step) * step if step > 0 else q


def orders(plan: dict, filters: tuple[dict, dict] | None = None) -> list[dict]:
    fut, spot = filters or _filters()
    out = []

    def pair(s: str, n: float, side: str, why: str):
        pp, sp = plan["prices"][s]
        fs, fm = _step_min(fut.get(s, {}), False)
        ss, sm = _step_min(spot.get(s, {}), True)
        q = min(_qty(n, sp, ss), _qty(n, pp, fs))                    # equal quantity on both legs
        ok = q * sp >= sm and q * pp >= fm
        out.append({"symbol": s, "why": why, "spot": ("BUY" if side == "open" else "SELL", q),
                    "perp": ("SELL" if side == "open" else "BUY", q), "notional": q * sp,
                    "margin_to_futures": P["buffer"] * q * pp if side == "open" else 0.0,
                    "ok": ok, "min": (sm, fm)})

    for s in plan["open"]:
        pair(s, plan["notional"][s], "open", f"funding {plan['score'][s] * 100:.3f} %/8h")
    for s in plan["close"]:
        if s in plan["prices"]:
            pair(s, plan["held_n"].get(s, 0.0), "close", "funding below 0.01 %/8h or out of the book")
    for s, (old, new) in plan["resize"].items():
        pair(s, abs(new - old), "open" if new > old else "close", f"resize {old:.2f} -> {new:.2f}")
    return out
