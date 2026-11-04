"""Trade lifecycle and risk limits, shared by paper and live mode.

A Slot is one symbol's trade: entry working -> open (protected by a stop + reduce-only takes, one per part:
tp1 / tp2 / run, see setup.py) -> closed. The stop always closes whatever is left of the position.
The broker (sim.SimBroker or live.LiveBroker) executes and reports back through on_entry_result / on_closed.
`tick()` is called every 0.5 s by the runner loop; the HTTP API reads `view()` from another thread.
"""
from __future__ import annotations

import asyncio
import logging
import math
import os
import threading
import time
from dataclasses import dataclass, field

from . import setup
from .book import SymbolBook
from .state import Store, utc_day

log = logging.getLogger("bounce")

RISK = {
    "notional": 10.0,          # USDT per trade
    "max_open": 2,             # positions + working entries at once
    "day_loss": 1.0,           # no new entries for the UTC day after -1 USDT
    "total_loss": 5.0,         # halt for good after -5 USDT since the record started
    "max_consec": 6,           # 6 losses in a row -> no new entries until the next UTC day
    "max_trades_day": 60,
    "sym_cooldown": 60.0,      # s after a closed trade before the same symbol trades again
    "attempts_per_density": 2,
    "leverage": 2,
}
FEE = {"maker": 0.0002, "taker": 0.0005}
_ENV = {"notional": "NOTIONAL", "max_open": "MAX_OPEN", "day_loss": "DAY_LOSS", "total_loss": "TOTAL_LOSS",
        "max_consec": "MAX_CONSEC_LOSSES", "max_trades_day": "MAX_TRADES_DAY", "leverage": "LEVERAGE"}


def load_risk() -> dict:
    for k, env in _ENV.items():
        v = os.environ.get(env, "").strip()
        if v:
            RISK[k] = type(RISK[k])(float(v))
    return RISK


@dataclass
class Slot:
    sym: str
    plan: setup.Plan
    qty: float
    state: str = "entry"           # entry | open | closing
    placed_at: float = 0.0
    filled_qty: float = 0.0
    entry_px: float = 0.0
    entry_fee: float = 0.0
    opened_at: float = 0.0
    stop: float = 0.0
    manage: setup.Manage | None = None
    cancel_sent: bool = False
    close_sent: bool = False
    oid: str = ""                  # client order id of the entry (live)
    parts: list = field(default_factory=list)    # [{name, px, qty, done, id, fill_px}] of the takes
    extra: dict = field(default_factory=dict)


def stop_reason(s: Slot) -> str:
    """The name of a stop-out: after the runner lock, after break-even, or the initial stop."""
    if s.extra.get("lock_done"):
        return "stop_lock"
    return "stop_be" if s.manage and s.manage.be_done else "stop"


class Trader:
    def __init__(self, broker, store: Store, info: dict[str, dict] | None = None, notify_trades: str = "closes"):
        self.broker = broker
        self.store = store
        self.info: dict[str, dict] = info or {}      # symbol -> {tick, step, min_qty, min_notional}
        self.books: dict[str, SymbolBook] = {}
        self.slots: dict[str, Slot] = {}
        self.attempts: dict[tuple, int] = {}         # (sym, dkey) -> attempts
        self.cool: dict[str, float] = {}
        self.blocked_syms: set[str] = set()          # live: symbols with a position / order the bot did not place
        self.halt = ""                               # set -> no new entries, ever (total loss)
        self.notify_trades = notify_trades           # closes | all | none
        self.lock = threading.Lock()                 # guards `slots` for the API thread
        self.last_block = ""
        broker.trader = self

    # ------------------------------------------------------------------ helpers
    def paused(self) -> bool:
        return self.store.data["desired"] == "paused"

    def _stat(self, key: str) -> None:
        with self.store.lock:
            self.store.data["stats"][key] = self.store.data["stats"].get(key, 0) + 1

    def size(self, sym: str, price: float) -> float:
        i = self.info[sym]
        step = i["step"]
        q = round(math.floor(RISK["notional"] / price / step + 1e-9) * step, 10)
        if q < i["min_qty"] or q * price < i["min_notional"]:
            q = round(math.ceil(i["min_notional"] / price / step - 1e-9) * step, 10)
        if q * price > RISK["notional"] * 1.6:
            return 0.0                               # step too coarse for this notional
        return q

    def split(self, s: Slot) -> list[dict]:
        """The takes of a filled position: tp1 / tp2 / run quantities on the lot grid; a part too small for the
        exchange (min qty, min notional + 10 %) is merged into tp2, so a small position keeps a single take."""
        i = self.info[s.sym]
        step, q, px = i["step"], s.filled_qty, s.entry_px

        def floor(x: float) -> float:
            return round(math.floor(x / step + 1e-9) * step, 10)

        def ok(x: float) -> bool:
            return x >= i["min_qty"] and x * px >= 1.1 * i["min_notional"]
        p = setup.P
        q1 = floor(q * p["tp1_part"]) if s.plan.tp1 else 0.0
        q3 = floor(q * (1 - p["tp1_part"] - p["tp2_part"])) if s.plan.tp_run else 0.0
        q1, q3 = (q1 if ok(q1) else 0.0), (q3 if ok(q3) else 0.0)
        q2 = round(q - q1 - q3, 10)
        if q2 <= 0 or not ok(q2):
            q1 = q3 = 0.0
            q2 = q
        parts = [("tp1", s.plan.tp1, q1), ("tp2", s.plan.tp, q2), ("run", s.plan.tp_run, q3)]
        return [{"name": n, "px": x, "qty": v, "done": False, "id": "", "fill_px": 0.0} for n, x, v in parts if v > 0]

    def block(self, now: float) -> str:
        """Why no new entry may open now ('' = free)."""
        d = self.store.data
        day = utc_day(now)
        if d["totals"]["pnl"] <= -RISK["total_loss"]:
            return f"total loss {d['totals']['pnl']:.2f} <= -{RISK['total_loss']:g} USDT: halted"
        today = d["days"].get(day, {})
        if today.get("pnl", 0.0) <= -RISK["day_loss"]:
            return f"day loss {today['pnl']:.2f} <= -{RISK['day_loss']:g} USDT"
        limit = d.get("consec_block_limit", 0)       # raising MAX_CONSEC_LOSSES lifts a block set under a lower limit
        if d.get("consec_block_day") == day and limit >= RISK["max_consec"]:
            return f"{limit} losses in a row today"
        if today.get("n", 0) >= RISK["max_trades_day"]:
            return f"{RISK['max_trades_day']} trades today"
        return ""

    # ------------------------------------------------------------------ main step
    async def tick(self, now: float) -> None:
        for sym, b in list(self.books.items()):
            s = self.slots.get(sym)
            if not b.synced or now - b.last_ts > 5:              # blind book: no new decisions
                if s and s.state == "entry" and not s.cancel_sent:
                    s.cancel_sent = True
                    s.extra["cancel_reason"] = "stale_book"
                    await self.broker.cancel_entry(s)
                continue
            b.scan_densities(now)
            try:
                if s is None:
                    await self._seek(sym, b, now)
                elif s.state == "entry":
                    await self._watch_entry(s, b, now)
                elif s.state == "open":
                    await self._watch_open(s, b, now)
            except Exception as e:                               # never let one symbol kill the loop
                log.exception("tick %s: %s", sym, e)
                self.store.event("error", f"{sym}: tick: {type(e).__name__}: {e}")

    async def _seek(self, sym: str, b: SymbolBook, now: float) -> None:
        if self.halt or self.paused() or sym in self.blocked_syms or now < self.cool.get(sym, 0):
            return
        if len(self.slots) >= RISK["max_open"]:
            return
        why = self.block(now)
        if why != self.last_block:
            self.last_block = why
            if why:
                self.store.event("warn", f"no new entries: {why}", True)
        if why:
            if "halted" in why:
                self.halt = why
            return
        skip = {k[1] for k, n in self.attempts.items() if k[0] == sym and n >= RISK["attempts_per_density"]}
        plan = setup.plan_bounce(b, now, skip)
        if plan is None:
            return
        self._stat("plans")
        qty = self.size(sym, plan.entry)
        if qty <= 0:
            self.cool[sym] = now + 600
            return
        s = Slot(sym, plan, qty, placed_at=now)
        with self.lock:
            self.slots[sym] = s
        self.attempts[(sym, plan.dkey)] = self.attempts.get((sym, plan.dkey), 0) + 1
        log.info("PLAN %s %s entry %g stop %g tp %g qty %g %s", sym, plan.side, plan.entry, plan.stop, plan.tp,
                 qty, plan.features)
        try:
            await self.broker.place_entry(s)
            self._stat("placed")
        except Exception as e:
            self._stat("rejected")
            log.info("%s entry rejected: %s", sym, e)
            with self.lock:
                self.slots.pop(sym, None)
            self.cool[sym] = now + 30

    async def _watch_entry(self, s: Slot, b: SymbolBook, now: float) -> None:
        if s.cancel_sent:
            return
        why = "halt" if self.halt else setup.cancel_reason(b, s.plan, s.placed_at, now)
        if why:
            s.cancel_sent = True
            s.extra["cancel_reason"] = why
            await self.broker.cancel_entry(s)

    async def _watch_open(self, s: Slot, b: SymbolBook, now: float) -> None:
        if s.close_sent or s.manage is None:
            return
        done = {p["name"] for p in s.parts if p["done"]}
        names = {p["name"] for p in s.parts}
        if "tp2" in done and "run" in names and not s.extra.get("lock_done"):
            s.extra["lock_done"] = True                            # the runner keeps the profit of tp1
            tp1 = next((p["px"] for p in s.parts if p["name"] == "tp1"), 0.0)
            px = tp1 or setup.be_price(s.plan, s.entry_px, b.tick_size)
            s.manage.be_done = True
            await self._move_stop(s, px, "stop_lock")
            return
        why = setup.exit_reason(b, s.plan, s.entry_px, s.manage, now)
        if why == "be" or ("tp1" in done and not s.manage.be_done):
            s.manage.be_done = True
            await self._move_stop(s, setup.be_price(s.plan, s.entry_px, b.tick_size), "stop_be")
        elif why:
            await self.close(s, why)

    async def _move_stop(self, s: Slot, px: float, why: str) -> None:
        try:
            await self.broker.move_stop(s, px)
            s.stop = px
            log.info("%s stop -> %g (%s)", s.sym, px, why)
        except Exception as e:
            if getattr(e, "code", None) == -2021:                 # price is already past the new stop
                await self.close(s, why)
                return
            self.store.event("error", f"{s.sym}: move stop: {e}")

    def on_part(self, sym: str, name: str, qty: float, px: float) -> None:
        """A partial take filled (the broker reports each part once)."""
        s = self.slots.get(sym)
        if s is None:
            return
        for p in s.parts:
            if p["name"] == name and not p["done"]:
                p["done"], p["fill_px"] = True, px
                sgn = 1 if s.plan.side == "long" else -1
                r = abs(s.entry_px - s.plan.stop) or s.plan.risk
                self.store.event("info", f"{s.plan.side} {sym}: {name} {qty:g} @ {px:g} "
                                         f"({sgn * (px - s.entry_px) / r:+.2f}R)", self.notify_trades == "all")

    async def close(self, s: Slot, why: str) -> None:
        if s.close_sent:
            return
        s.close_sent = True
        s.state = "closing"
        s.extra["exit_reason"] = why
        try:
            await self.broker.close(s, why)
        except Exception as e:
            log.exception("close %s failed: %s", s.sym, e)
            self.store.event("error", f"{s.sym}: close failed: {e}")
            s.close_sent = False                                 # retry on the next tick
            s.state = "open"

    async def flatten(self) -> int:
        """Cancel every working entry and close every open position of the bot. Returns the slots touched."""
        n = 0
        for s in list(self.slots.values()):
            n += 1
            if s.state == "entry" and not s.cancel_sent:
                s.cancel_sent = True
                s.extra["cancel_reason"] = "flatten"
                await self.broker.cancel_entry(s)
            elif s.state == "open":
                await self.close(s, "flatten")
        return n

    # ------------------------------------------------------------------ broker callbacks
    def on_entry_result(self, sym: str, filled: float, avg_px: float, fee: float) -> None:
        s = self.slots.get(sym)
        if s is None or s.state != "entry":
            return
        if filled <= 0:
            self._stat("cancelled")
            with self.lock:
                self.slots.pop(sym, None)
            self.cool[sym] = time.time() + 5
            return
        s.filled_qty, s.entry_px, s.entry_fee = filled, avg_px, fee
        s.state, s.opened_at, s.stop = "open", time.time(), s.plan.stop
        s.manage = setup.Manage(opened_at=s.opened_at)
        s.parts = self.split(s)
        self._stat("filled")
        parts = ", ".join(f"{p['name']} {p['qty']:g} @ {p['px']:g}" for p in s.parts)
        self.store.event("info", f"opened {s.plan.side} {sym} {filled:g} @ {avg_px:g}, stop {s.plan.stop:g}, "
                                 f"take {s.plan.tp:g} (density {s.plan.features.get('density_usd', 0):,} USDT); "
                                 f"parts {parts}", self.notify_trades == "all")
        asyncio.get_event_loop().create_task(self._protect(s))

    async def _protect(self, s: Slot) -> None:
        try:
            await self.broker.protect(s)
        except Exception as e:
            log.error("PROTECT FAILED %s: %s -> closing at market", s.sym, e)
            self.store.event("error", f"{s.sym}: stop not placed ({e}) -> closing at market", True)
            await self.close(s, "protect_failed")

    def on_closed(self, sym: str, exit_px: float, pnl: float, fees: float, why: str = "") -> None:
        with self.lock:
            s = self.slots.pop(sym, None)
        if s is None:
            return
        now = time.time()
        sgn = 1 if s.plan.side == "long" else -1
        r_usd = abs(s.entry_px - s.plan.stop) * s.filled_qty
        reason = why or s.extra.get("exit_reason", "")
        tr = {"ts_open": round(s.opened_at, 3), "ts_close": round(now, 3), "sym": sym, "side": s.plan.side,
              "entry": s.entry_px, "exit": exit_px, "qty": s.filled_qty,
              "notional": round(s.entry_px * s.filled_qty, 4), "stop": s.plan.stop, "tp": s.plan.tp,
              "reason": reason, "gross": round(sgn * (exit_px - s.entry_px) * s.filled_qty, 6),
              "fees": round(fees, 6), "pnl": round(pnl, 6), "r": round(pnl / r_usd, 3) if r_usd else None,
              "hold_s": round(now - s.opened_at, 1), "features": s.plan.features,
              "parts": [{k: p[k] for k in ("name", "px", "qty", "done", "fill_px")} for p in s.parts]}
        self.store.record_trade(tr)
        d = self.store.data
        if d["consec_losses"] >= RISK["max_consec"]:
            self.store.set("consec_block_day", utc_day(now))
            self.store.set("consec_block_limit", RISK["max_consec"])
            self.store.set("consec_losses", 0)
        self.cool[sym] = now + RISK["sym_cooldown"]
        icon = "✅" if pnl > 0 else "❌"
        hit = " ".join(p["name"] for p in s.parts if p["done"])
        self.store.event("info", f"{icon} {s.plan.side} {sym} closed ({reason}) {s.entry_px:g} -> {exit_px:g}: "
                                 f"{pnl:+.4f} USDT ({tr['r']:+.2f}R), held {tr['hold_s']:.0f} s"
                                 + (f", parts {hit}" if hit and len(s.parts) > 1 else "")
                         if tr["r"] is not None else f"{icon} {sym} closed ({reason}): {pnl:+.4f} USDT",
                         self.notify_trades != "none")

    # ------------------------------------------------------------------ views (API thread)
    def view(self, marks: dict[str, float] | None = None) -> list[dict]:
        with self.lock:
            slots = list(self.slots.values())
        out = []
        for s in slots:
            b = self.books.get(s.sym)
            mid = (b.mid() if b else None) or (marks or {}).get(s.sym)
            sgn = 1 if s.plan.side == "long" else -1
            upnl = sgn * (mid - s.entry_px) * s.filled_qty - s.entry_fee if (mid and s.state != "entry") else 0.0
            out.append({"symbol": s.sym, "side": s.plan.side, "state": s.state, "entry": s.entry_px or s.plan.entry,
                        "stop": s.stop or s.plan.stop, "tp": s.plan.tp, "qty": s.filled_qty or s.qty,
                        "n": round((s.entry_px or s.plan.entry) * (s.filled_qty or s.qty), 2), "mark": mid,
                        "upnl": round(upnl, 4), "age_s": round(time.time() - (s.opened_at or s.placed_at)),
                        "features": s.plan.features})
        return out
