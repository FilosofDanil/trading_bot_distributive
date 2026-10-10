"""Paper execution: the plan's book becomes the held state (lab carry_live `accept`), and every day the held
pairs are marked like lab/carry.run_carry does:

  daily PnL per pair = funding settled on the short since the last mark (+rate x N)
                       + basis change on N (spot leg minus perp leg, open to open)
                       - fees when a pair is opened, closed or resized (FEE_SIDE per unit of N traded)

Fills are assumed at the daily open used by the decision; no order is ever sent.
"""
from __future__ import annotations

from .market import DAY_MS
from .strategy import FEE_SIDE


def _day(ms: int) -> str:
    import time
    return time.strftime("%Y-%m-%d", time.gmtime(ms / 1000))


def funding_income(rates: list[tuple[int, float]], since: int, until: int, n: float) -> float:
    """Funding received by a short of notional n for settlements in [since, until)."""
    return sum(rate * n for t, rate in rates if since <= t < until)


def mark(held: dict[str, dict], prices: dict[str, tuple[float, float]], rates: dict[str, list[tuple[int, float]]],
         day: int) -> dict[str, dict]:
    """PnL of every held pair from its last mark to `day` (prices = today's (perp, spot) open)."""
    out = {}
    for s, h in held.items():
        n = h["n"]
        since = h.get("marked", day)
        fund = funding_income(rates.get(s, []), since, day, n)
        if s in prices:
            perp, spot = prices[s]
            basis = n * (spot / h["s_last"] - 1) - n * (perp / h["p_last"] - 1)
        else:
            perp, spot, basis = h["p_last"], h["s_last"], 0.0     # no price today: carried, flagged by the caller
        out[s] = {"funding": fund, "basis": basis, "perp": perp, "spot": spot, "priced": s in prices}
    return out


def apply(state: dict, plan: dict, marks: dict[str, dict]) -> dict:
    """Book the marks, fees and the new book into `state` (mutated). Returns the day's summary."""
    day = plan["day"]
    held = state["held"]
    stats = state.setdefault("stats", {"pairs_opened": 0, "pairs_closed": 0, "pair_stops": 0})
    fund = sum(m["funding"] for m in marks.values())
    basis = sum(m["basis"] for m in marks.values())
    for s, m in marks.items():
        h = held[s]
        h["funding"] = h.get("funding", 0.0) + m["funding"]
        h["basis"] = h.get("basis", 0.0) + m["basis"]
        h["p_last"], h["s_last"], h["marked"] = m["perp"], m["spot"], day

    fees = 0.0
    events: list[tuple[str, str, bool]] = []
    new_held: dict[str, dict] = {}
    for s in plan["book"]:
        old = held.get(s)
        perp, spot = plan["prices"][s]
        n = plan["notional"][s]
        if old is None:
            fee = FEE_SIDE * n
            new_held[s] = {"n": n, "ref": perp, "perp0": perp, "spot0": spot, "p_last": perp, "s_last": spot,
                           "marked": day, "opened": day, "funding": 0.0, "basis": 0.0, "fees": fee}
            stats["pairs_opened"] += 1
            events.append(("info", f"paper: opened {s} pair {n:.2f} USDT per leg (funding "
                                   f"{(plan['score'].get(s) or 0) * 100:.3f} %/8h)", True))
        else:
            fee = FEE_SIDE * abs(n - old["n"])
            h = dict(old)
            h["n"] = n
            if s in plan["rehedge"]:
                h["ref"] = perp
                fee += FEE_SIDE * n
                events.append(("warn", f"paper: re-hedged {s} (perp up >= +100 % since entry)", True))
            h["fees"] = h.get("fees", 0.0) + fee
            new_held[s] = h
            if abs(n - old["n"]) > 1e-9:
                events.append(("info", f"paper: resized {s} {old['n']:.2f} -> {n:.2f} USDT", False))
        fees += fee
    for s, h in held.items():
        if s in new_held:
            continue
        fee = FEE_SIDE * h["n"]
        fees += fee
        h["fees"] = h.get("fees", 0.0) + fee
        stopped = s in plan["pair_stopped"]
        reason = "pair stop (basis -2 %)" if stopped else plan["skipped"].get(s, "funding below 0.01 %/8h or out of the book")
        pnl = h.get("funding", 0.0) + h.get("basis", 0.0) - h["fees"]
        state.setdefault("closed", []).append({"symbol": s, "opened": _day(h.get("opened", day)), "closed": _day(day),
                                               "n": h["n"], "funding": h.get("funding", 0.0), "basis": h.get("basis", 0.0),
                                               "fees": h["fees"], "pnl": pnl, "reason": reason})
        state["closed"] = state["closed"][-200:]
        stats["pairs_closed"] += 1
        stats["pair_stops"] += stopped
        priced = marks.get(s, {}).get("priced", True)
        events.append(("warn" if stopped or not priced else "info",
                       f"paper: closed {s} ({reason}), pnl {pnl:+.2f} USDT"
                       + ("" if priced else " - no price today, closed at the last mark"), True))

    cool = {s: d for s, d in state.get("cool", {}).items() if d > day}
    cool.update({s: day + 7 * DAY_MS for s in plan.get("pair_stopped", [])})
    equity = state["capital"] + fund + basis - fees
    state.update({"held": new_held, "capital": equity, "cool": cool})
    row = {"day": _day(day), "equity": equity, "funding": fund, "basis": basis, "fees": -fees, "book": list(plan["book"])}
    state.setdefault("daily", []).append(row)
    state["daily"] = state["daily"][-400:]
    state.setdefault("days", []).append(_day(day))
    state["days"] = state["days"][-400:]
    return {"row": row, "events": events}
