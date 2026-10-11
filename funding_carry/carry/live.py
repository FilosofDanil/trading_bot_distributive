"""Live execution of the carry plan and the intraday liquidation guard (MODE=live).

The decision is the same `strategy.decide`; here its plan is carried out with real orders on the owner's
Binance account, through the `Exchange` interface (carry/exchange.py implements it for Binance; tests use a
fake). A pair is a spot long and an isolated 1x perp short of the same quantity q.

Opening a pair (in this order, each step undone if a later one fails):
  1. move the perp margin (buffer x notional = 2N) from spot to the futures wallet
  2. isolated margin, leverage 1; market SELL q on the perp
  3. market BUY on spot, q grossed up for the taker fee charged in the coin
  4. add the rest of the margin to the isolated position (total 2N: liquidation about +195 %)
  5. exchange-side STOP_MARKET that closes the short at ref x (1 + stop_rise): works while the bot is down

Liquidation protection, checked every WATCH_EVERY_S seconds (watch_once), for each held pair:
  L1 liquidation price within topup_dist above the mark -> add isolated margin (futures wallet, then spot)
  L2 perp up close_rise since ref, or liquidation within close_dist -> close the pair, 7-day cooldown
  L3 the exchange stop is missing -> place it again (or close the pair if the price is already past it)
  L4 a leg is gone (stop fired, liquidated, sold by hand) -> close the other leg, 7-day cooldown
Every action is an event with notify=true, so control forwards it to Telegram.
"""
from __future__ import annotations

import math
import os
import threading
import time
from dataclasses import dataclass
from typing import Protocol

from . import strategy
from .market import DAY_MS
from .state import Store

SPOT_FEE = 0.001             # spot taker, charged in the bought coin on a BUY (unless paid in BNB)
PERP_FEE = 0.0005            # perp taker, in USDT
WATCH_EVERY_S = 30
COOLDOWN_MS = 7 * DAY_MS


def _env_float(name: str, default: float) -> float:
    try:
        return float(os.environ.get(name, default))
    except ValueError:
        return default


@dataclass
class Protect:
    topup_dist: float = 0.60    # L1: add margin when the liquidation price is less than +60 % above the mark
    close_dist: float = 0.25    # L2: close when it is less than +25 % above the mark
    close_rise: float = 0.90    # L2: close when the perp is +90 % above ref (entry / last re-hedge)
    stop_rise: float = 1.20     # L3: exchange stop at +120 % above ref
    leg_tol: float = 0.05       # L4: a leg below 95 % of its size counts as gone
    topup_every_s: float = 600  # at most one top-up per pair per 10 min

    @classmethod
    def from_env(cls) -> "Protect":
        p = cls()
        return cls(topup_dist=_env_float("LIQ_TOPUP_DIST", p.topup_dist),
                   close_dist=_env_float("LIQ_CLOSE_DIST", p.close_dist),
                   close_rise=_env_float("LIQ_CLOSE_RISE", p.close_rise),
                   stop_rise=_env_float("LIQ_STOP_RISE", p.stop_rise))


@dataclass
class Fill:
    qty: float        # base quantity: for a spot BUY the quantity received after the fee
    price: float      # average price
    fee_usdt: float   # fee in USDT terms


@dataclass
class Perp:
    amt: float        # signed position amount (short < 0)
    entry: float
    mark: float
    liq: float        # liquidation price (0 = none)
    margin: float     # isolated margin


class ExchangeError(RuntimeError):
    pass


class Exchange(Protocol):
    def prices(self, sym: str) -> tuple[float, float]: ...                   # (perp mark, spot last)
    def filters(self, sym: str) -> tuple[float, float, float, float]: ...     # spot step, spot min, perp step, perp min
    def spot_free(self, asset: str) -> float: ...
    def futures_available(self) -> float: ...                                 # USDT available in the futures wallet
    def transfer(self, amount: float, to_futures: bool) -> None: ...
    def prepare_short(self, sym: str) -> None: ...                            # isolated + leverage 1
    def perp_order(self, sym: str, side: str, qty: float, reduce: bool) -> Fill: ...
    def spot_order(self, sym: str, side: str, qty: float) -> Fill: ...
    def add_margin(self, sym: str, amount: float) -> None: ...
    def positions(self) -> dict[str, Perp]: ...                               # non-zero positions
    def stops(self, syms: list[str]) -> dict[str, list[str]]: ...            # this bot's open exchange stops
    def place_stop(self, sym: str, trigger: float) -> str: ...
    def cancel_stops(self, sym: str) -> None: ...
    def funding(self, sym: str, since_ms: int) -> float: ...                  # funding received since


def floor_step(q: float, step: float) -> float:
    if step <= 0:
        return q
    return math.floor(q / step + 1e-9) * step


def ceil_step(q: float, step: float) -> float:
    if step <= 0:
        return q
    return math.ceil(q / step - 1e-9) * step


def base_asset(sym: str) -> str:
    return sym[:-4] if sym.endswith("USDT") else sym


def _now_ms() -> int:
    return int(time.time() * 1000)


class Live:
    def __init__(self, ex: Exchange, store: Store, protect: Protect | None = None):
        self.ex = ex
        self.store = store
        self.p = protect or Protect()
        self.lock = threading.RLock()          # serialises the daily cycle, the watchdog and flatten
        self._topup_at: dict[str, float] = {}

    # ------------------------------------------------------------ helpers
    def _event(self, level: str, msg: str) -> None:
        self.store.event(level, f"live: {msg}", True)

    def _try(self, what: str, fn) -> bool:
        try:
            fn()
            return True
        except Exception as exc:                          # noqa: BLE001 - every failure is reported
            self._event("error", f"{what} failed: {type(exc).__name__}: {exc}")
            return False

    def _held(self) -> dict[str, dict]:
        return self.store.snapshot()["held"]

    def _save_pair(self, sym: str, h: dict | None, cool: bool = False) -> None:
        with self.store.lock:
            held = self.store.data["held"]
            if h is None:
                held.pop(sym, None)
                if cool:
                    self.store.data.setdefault("cool", {})[sym] = (_now_ms() // DAY_MS) * DAY_MS + COOLDOWN_MS
            else:
                held[sym] = h
            self.store.save()

    # ------------------------------------------------------------ pair operations
    def open_pair(self, sym: str, n: float, why: str) -> dict | None:
        perp_px, spot_px = self.ex.prices(sym)
        ss, smin, ps, pmin = self.ex.filters(sym)
        q = min(floor_step(n / spot_px, ss), floor_step(n / perp_px, ps))
        if q <= 0 or q * spot_px < smin or q * perp_px < pmin:
            self._event("warn", f"{sym}: {n:.2f} USDT per leg is below the exchange minimum, not opened")
            return None
        if sym in self.ex.positions():
            # one-way mode would net the short against the owner's own position on this symbol
            self._event("warn", f"{sym}: the account already has a futures position on it, not opened")
            return None
        q_gross = ceil_step(q / (1 - SPOT_FEE), ss)
        margin = strategy.P["buffer"] * q * perp_px
        moved = round(margin * 1.01, 2)
        need = q_gross * spot_px * 1.003 + moved
        free = self.ex.spot_free("USDT")
        if free < need:
            self._event("warn", f"{sym}: needs {need:.2f} USDT on spot, {free:.2f} free - not opened")
            return None

        self.ex.transfer(moved, to_futures=True)
        try:
            self.ex.prepare_short(sym)
            perp = self.ex.perp_order(sym, "SELL", q, reduce=False)
        except Exception as exc:                          # noqa: BLE001
            self._try(f"{sym}: return {moved:.2f} USDT to spot", lambda: self.ex.transfer(moved, to_futures=False))
            self._event("error", f"{sym}: perp short failed ({type(exc).__name__}: {exc}), pair not opened")
            return None
        try:
            spot = self.ex.spot_order(sym, "BUY", q_gross)
        except Exception as exc:                          # noqa: BLE001
            self._try(f"{sym}: unwind the perp short", lambda: self.ex.perp_order(sym, "BUY", perp.qty, reduce=True))
            self._try(f"{sym}: return the margin to spot",
                      lambda: self.ex.transfer(min(moved, self.ex.futures_available()), to_futures=False))
            self._event("error", f"{sym}: spot buy failed ({type(exc).__name__}: {exc}), the short was unwound")
            return None

        extra = min(max(0.0, (strategy.P["buffer"] - 1) * perp.qty * perp.price), self.ex.futures_available())
        if extra > 0 and not self._try(f"{sym}: add {extra:.2f} USDT isolated margin",
                                       lambda: self.ex.add_margin(sym, round(extra, 2))):
            extra = 0.0
        trigger = perp.price * (1 + self.p.stop_rise)
        self._try(f"{sym}: exchange stop at {trigger:.6g}", lambda: self.ex.place_stop(sym, trigger))

        now = _now_ms()
        h = {"n": perp.qty * perp.price, "q": perp.qty, "q_spot": spot.qty, "ref": perp.price,
             "perp0": perp.price, "spot0": spot.price, "spot_cost": q_gross * spot.price,
             "margin": moved, "opened": now, "marked": now, "fees": perp.fee_usdt + spot.fee_usdt,
             "funding": 0.0, "basis": 0.0, "p_last": perp.price, "s_last": spot.price}
        self._save_pair(sym, h)
        self._event("info", f"opened {sym}: short {perp.qty:g} @ {perp.price:.6g}, spot {spot.qty:g} @ {spot.price:.6g}, "
                            f"{h['n']:.2f} USDT per leg, margin {perp.qty * perp.price + extra:.2f} ({why})")
        return h

    def close_pair(self, sym: str, h: dict, reason: str, cool: bool = False) -> dict | None:
        """Close both legs. Returns the closed record, or None if the perp could not be closed (pair kept)."""
        self._try(f"{sym}: cancel the exchange stop", lambda: self.ex.cancel_stops(sym))
        pos = self.ex.positions().get(sym)
        perp_fill = None
        if pos is not None and pos.amt < 0:
            try:
                perp_fill = self.ex.perp_order(sym, "BUY", abs(pos.amt), reduce=True)
            except Exception as exc:                      # noqa: BLE001
                self._event("error", f"{sym}: closing the short failed ({type(exc).__name__}: {exc}); "
                                     f"the pair is kept and the next check retries")
                return None
        perp_px, spot_px = self.ex.prices(sym)
        ss, smin, _, _ = self.ex.filters(sym)
        qs = floor_step(min(self.ex.spot_free(base_asset(sym)), h["q_spot"]), ss)
        spot_fill = None
        if qs > 0 and qs * spot_px >= smin:
            try:
                spot_fill = self.ex.spot_order(sym, "SELL", qs)
            except Exception as exc:                      # noqa: BLE001
                self._event("error", f"{sym}: selling {qs:g} on spot failed ({type(exc).__name__}: {exc}); sell it by hand")
        elif qs > 0:
            self._event("warn", f"{sym}: {qs:g} left on spot is below the minimum order, kept as dust")

        exit_px = perp_fill.price if perp_fill else perp_px
        perp_pnl = (h["ref"] - exit_px) * h["q"] if perp_fill else 0.0
        back = min(self.ex.futures_available(), max(0.0, h["margin"] + perp_pnl))
        if back >= 1:
            self._try(f"{sym}: return {back:.2f} USDT to spot", lambda: self.ex.transfer(round(back, 2), to_futures=False))
        funding = self._safe_funding(sym, h["opened"])
        fees = h.get("fees", 0.0) + (perp_fill.fee_usdt if perp_fill else 0.0) + (spot_fill.fee_usdt if spot_fill else 0.0)
        proceeds = spot_fill.qty * spot_fill.price if spot_fill else 0.0
        pnl = proceeds - h["spot_cost"] + perp_pnl + h.get("realized", 0.0) + funding - fees
        rec = {"symbol": sym, "opened": time.strftime("%Y-%m-%d", time.gmtime(h["opened"] / 1000)),
               "closed": time.strftime("%Y-%m-%d", time.gmtime()), "n": h["n"], "funding": funding,
               "basis": pnl - funding + fees, "fees": fees, "pnl": pnl, "reason": reason}
        with self.store.lock:
            d = self.store.data
            d["capital"] = d["capital"] + pnl - h.get("booked", 0.0)
            d.setdefault("closed", []).append(rec)
            d["closed"] = d["closed"][-200:]
            st = d.setdefault("stats", {"pairs_opened": 0, "pairs_closed": 0, "pair_stops": 0})
            st["pairs_closed"] += 1
            st["pair_stops"] += "stop" in reason
        self._save_pair(sym, None, cool=cool)
        self._event("warn" if cool else "info", f"closed {sym} ({reason}), pnl {pnl:+.2f} USDT")
        return rec

    def resize_pair(self, sym: str, h: dict, new_n: float) -> dict:
        perp_px, spot_px = self.ex.prices(sym)
        ss, smin, ps, pmin = self.ex.filters(sym)
        dq = min(floor_step(abs(new_n - h["n"]) / spot_px, ss), floor_step(abs(new_n - h["n"]) / perp_px, ps))
        if dq <= 0 or dq * spot_px < smin or dq * perp_px < pmin:
            return h
        h = dict(h)
        if new_n > h["n"]:
            dq_gross = ceil_step(dq / (1 - SPOT_FEE), ss)
            add = strategy.P["buffer"] * dq * perp_px
            if self.ex.spot_free("USDT") < dq_gross * spot_px * 1.003 + add:
                self._event("warn", f"{sym}: not enough spot USDT to grow the pair, kept at {h['n']:.2f}")
                return h
            self.ex.transfer(round(add, 2), to_futures=True)
            perp = self.ex.perp_order(sym, "SELL", dq, reduce=False)
            try:
                spot = self.ex.spot_order(sym, "BUY", dq_gross)
            except Exception as exc:                      # noqa: BLE001
                self._try(f"{sym}: unwind the added short", lambda: self.ex.perp_order(sym, "BUY", perp.qty, reduce=True))
                self._event("error", f"{sym}: resize spot buy failed ({exc}), unwound")
                return h
            self._try(f"{sym}: add margin", lambda: self.ex.add_margin(sym, round(max(0.0, add - perp.qty * perp.price), 2)))
            h["ref"] = (h["ref"] * h["q"] + perp.price * perp.qty) / (h["q"] + perp.qty)
            h["q"] += perp.qty
            h["q_spot"] += spot.qty
            h["spot_cost"] += dq_gross * spot.price
            h["margin"] += round(add, 2)
            h["fees"] += perp.fee_usdt + spot.fee_usdt
        else:
            perp = self.ex.perp_order(sym, "BUY", dq, reduce=True)
            spot = self.ex.spot_order(sym, "SELL", floor_step(min(dq, h["q_spot"]), ss))
            share = dq / h["q"]
            h["realized"] = h.get("realized", 0.0) + (h["ref"] - perp.price) * dq + spot.qty * spot.price - h["spot_cost"] * share
            h["spot_cost"] *= 1 - share
            h["q"] -= dq
            h["q_spot"] -= spot.qty
            h["margin"] *= 1 - share
            h["fees"] += perp.fee_usdt + spot.fee_usdt
        h["n"] = h["q"] * h["ref"]
        self._save_pair(sym, h)
        self._event("info", f"resized {sym} to {h['n']:.2f} USDT per leg")
        return h

    # ------------------------------------------------------------ the daily plan
    def execute(self, plan: dict) -> None:
        """Carry out an accepted plan: closes, re-hedges, resizes, then opens."""
        with self.lock:
            held = self._held()
            for s in plan["close"]:
                if s in held:
                    stopped = s in plan["pair_stopped"]
                    reason = "pair stop (basis -2 %)" if stopped else plan["skipped"].get(
                        s, "funding below 0.01 %/8h or out of the book")
                    self.close_pair(s, held[s], reason, cool=stopped)
            held = self._held()
            for s in plan["rehedge"]:
                if s in held and self.close_pair(s, held[s], "re-hedge (perp +100 % since entry)") is not None:
                    self.open_pair(s, plan["notional"][s], "re-hedge")
            held = self._held()
            for s, (_, new) in plan["resize"].items():
                if s in held and s not in plan["rehedge"]:
                    self._try(f"{s}: resize", lambda s=s, new=new: self.resize_pair(s, self._held()[s], new))
            for s in plan["open"]:
                if s not in self._held():
                    self._try(f"{s}: open", lambda s=s: self.open_pair(
                        s, plan["notional"][s], f"funding {(plan['score'].get(s) or 0) * 100:.3f} %/8h"))

    def mark(self, day: int) -> dict:
        """Mark every held pair to market; returns the day row (equity = capital booked + open PnL)."""
        held = self._held()
        pos = self.ex.positions()
        fund = basis = 0.0
        for s, h in held.items():
            perp_px, spot_px = self.ex.prices(s)
            p = pos.get(s)
            funding = self._safe_funding(s, h["opened"])
            perp_pnl = (h["ref"] - (p.mark if p else perp_px)) * h["q"] + h.get("realized", 0.0)
            spot_pnl = h["q_spot"] * spot_px - h["spot_cost"]
            fund += funding - h.get("funding", 0.0)
            basis += perp_pnl + spot_pnl - h.get("basis", 0.0)
            h.update(funding=funding, basis=perp_pnl + spot_pnl, p_last=perp_px, s_last=spot_px, marked=day)
        with self.store.lock:
            d = self.store.data
            for s, h in held.items():
                if s in d["held"]:
                    d["held"][s].update({k: h[k] for k in ("funding", "basis", "p_last", "s_last", "marked")})
            open_pnl = sum(h["funding"] + h["basis"] - h.get("fees", 0.0) for h in d["held"].values())
            for h in d["held"].values():
                h["booked"] = 0.0          # open PnL is not booked into capital until the pair closes
            equity = d["capital"] + open_pnl
            row = {"day": time.strftime("%Y-%m-%d", time.gmtime(day / 1000)), "equity": equity, "funding": fund,
                   "basis": basis, "fees": 0.0, "book": sorted(d["held"])}
            d.setdefault("daily", []).append(row)
            d["daily"] = d["daily"][-400:]
            self.store.save()
        return row

    def _safe_funding(self, sym: str, since: int) -> float:
        try:
            return self.ex.funding(sym, since)
        except Exception:                                 # noqa: BLE001 - funding is reporting, not safety
            return 0.0

    # ------------------------------------------------------------ the watchdog
    def watch_once(self) -> None:
        if not self.lock.acquire(blocking=False):         # the daily cycle is trading: next tick
            return
        try:
            held = self._held()
            if not held:
                return
            pos = self.ex.positions()
            stops = self.ex.stops(sorted(held))
            for s, h in held.items():
                self._watch_pair(s, h, pos.get(s), stops.get(s, []))
        finally:
            self.lock.release()

    def _watch_pair(self, s: str, h: dict, p: Perp | None, stops: list[str]) -> None:
        # L4: legs
        if p is None or p.amt > -(1 - self.p.leg_tol) * h["q"]:
            self.close_pair(s, h, "the perp leg is gone (exchange stop, liquidation or closed by hand)", cool=True)
            return
        if self.ex.spot_free(base_asset(s)) < (1 - self.p.leg_tol) * h["q_spot"]:
            self.close_pair(s, h, "the spot leg is gone (sold or moved by hand)", cool=True)
            return
        rise = p.mark / h["ref"] - 1
        dist = p.liq / p.mark - 1 if p.liq > 0 and p.mark > 0 else math.inf
        # L2: close before the liquidation
        if rise >= self.p.close_rise or dist < self.p.close_dist:
            self.close_pair(s, h, f"liquidation protection: perp {rise:+.0%} since entry, liquidation {dist:+.0%} away",
                            cool=True)
            return
        # L1: more margin
        if dist < self.p.topup_dist and time.time() - self._topup_at.get(s, 0) >= self.p.topup_every_s:
            self._topup(s, h, p, dist)
        # L3: the exchange stop
        if not stops:
            trigger = h["ref"] * (1 + self.p.stop_rise)
            if p.mark >= trigger:
                self.close_pair(s, h, "the exchange stop is missing and the price is past it", cool=True)
            elif self._try(f"{s}: re-place the exchange stop at {trigger:.6g}", lambda: self.ex.place_stop(s, trigger)):
                self._event("warn", f"{s}: the exchange stop was missing, placed again at {trigger:.6g}")

    def _topup(self, s: str, h: dict, p: Perp, dist: float) -> None:
        want = round(0.5 * h["q"] * p.mark, 2)           # half the current notional
        avail = self.ex.futures_available()
        if avail < want:
            spot_usdt = self.ex.spot_free("USDT")
            move = round(min(want - avail, spot_usdt), 2)
            if move >= 1 and self._try(f"{s}: move {move:.2f} USDT to futures", lambda: self.ex.transfer(move, True)):
                avail += move
        amount = round(min(want, avail), 2)
        self._topup_at[s] = time.time()
        if amount < 1:
            self._event("warn", f"{s}: liquidation {dist:+.0%} away and no USDT to add as margin")
            return
        if self._try(f"{s}: add {amount:.2f} USDT margin", lambda: self.ex.add_margin(s, amount)):
            h = dict(h)
            h["margin"] += amount
            self._save_pair(s, h)
            self._event("warn", f"{s}: liquidation was {dist:+.0%} away (perp {p.mark / h['ref'] - 1:+.0%}), "
                                f"added {amount:.2f} USDT isolated margin")

    # ------------------------------------------------------------ manual
    def flatten(self, by=None) -> int:
        """Close every pair now (kill switch). Returns the number closed."""
        n = 0
        with self.lock:
            for s, h in self._held().items():
                if self.close_pair(s, h, f"close all by {by}" if by else "close all") is not None:
                    n += 1
        return n

    def reconcile(self) -> None:
        """At start: report pairs whose legs are not on the exchange (the watchdog then closes the other leg)."""
        held = self._held()
        if not held:
            return
        pos = self.ex.positions()
        missing = [s for s in held if s not in pos]
        if missing:
            self._event("warn", f"at start the perp leg is missing for {', '.join(missing)}; the watchdog closes "
                                f"the spot leg")
