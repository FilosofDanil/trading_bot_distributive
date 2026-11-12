"""SimBroker: paper mode. Live market data, simulated fills, no orders sent (from the lab's lab/book/sim.py).

Fill model (conservative for a small order):
- post-only entry limit: rejected if it would cross; filled when a trade prints THROUGH the price, or when the
  volume traded AT the price after placement exceeds the queue that was resting there when we joined;
- stop (last price): triggered by the first trade at/through it, filled at that trade price - 0.02 % slippage;
- reduce-only take limits (one per part): the same queue rule as the entry, each with its own queue;
- market close: best opposite price - 0.01 %.
Fees: maker 0.02 %, taker 0.05 %. Latency: SIM_LAG (0.3 s) on placing and cancelling an entry (v0.4.5); the stop
and the takes have none, so paper results are still an upper bound.
"""
from __future__ import annotations

import asyncio
from typing import Any

from .book import SymbolBook, Trade
from .trader import FEE, Slot, stop_reason


class SimBroker:
    def __init__(self, lag: float = 0.0):
        """`lag` (s) is the round trip to the exchange: a new entry starts to count the tape that late, and a
        cancel takes effect that late, so the order can still fill meanwhile, as a live cancel can. Without it
        the paper record credits every "density is being eaten" cancel with a perfect escape."""
        self.trader: Any = None                 # set by Trader()
        self.o: dict[str, dict] = {}            # sym -> working order state
        self.lag = lag

    def _book(self, sym: str) -> SymbolBook:
        return self.trader.books[sym]

    async def place_entry(self, s: Slot) -> None:
        b = self._book(s.sym)
        long = s.plan.side == "long"
        e = b.t(s.plan.entry)
        bb, ba = b.best_bid(), b.best_ask()
        if bb is None or ba is None or (long and e >= ba) or (not long and e <= bb):
            raise RuntimeError("post-only would cross")
        self.o[s.sym] = {"kind": "entry", "tick": e, "queue": b.qty_at("bid" if long else "ask", e), "traded": 0.0,
                         "live_at": b.last_ts + self.lag}

    def _cancel_done(self, s: Slot) -> None:
        o = self.o.get(s.sym)
        if o and o["kind"] == "entry":
            del self.o[s.sym]
        self.trader.on_entry_result(s.sym, 0, 0, 0)          # ignored when the entry filled meanwhile

    async def cancel_entry(self, s: Slot) -> None:
        if self.lag > 0:
            asyncio.get_running_loop().call_later(self.lag, self._cancel_done, s)
        else:
            self._cancel_done(s)

    async def protect(self, s: Slot) -> None:
        b = self._book(s.sym)
        side = "ask" if s.plan.side == "long" else "bid"
        takes = [{"name": p["name"], "qty": p["qty"], "tick": b.t(p["px"]), "queue": b.qty_at(side, b.t(p["px"])),
                  "traded": 0.0} for p in s.parts]
        # left = position still open; gross / fees / xn (exit notional) of the parts already taken
        self.o[s.sym] = {"kind": "open", "stop": b.t(s.plan.stop), "takes": takes, "left": s.filled_qty,
                         "gross": 0.0, "fees": 0.0, "xn": 0.0}

    async def move_stop(self, s: Slot, price: float) -> None:
        o = self.o.get(s.sym)
        if o and o["kind"] == "open":
            o["stop"] = self._book(s.sym).t(price)

    async def close(self, s: Slot, why: str) -> None:
        b = self._book(s.sym)
        long = s.plan.side == "long"
        best = b.best_bid() if long else b.best_ask()
        px = (b.p(best) if best is not None else s.entry_px) * (1 - 0.0001 if long else 1 + 0.0001)
        self._finish(s, px, FEE["taker"], why)

    def _finish(self, s: Slot, px: float, fee_rate: float, why: str) -> None:
        """Close what is left at `px`; the trade's exit is the average over the parts taken before."""
        o = self.o.pop(s.sym, None) or {}
        sgn = 1 if s.plan.side == "long" else -1
        q = o.get("left", s.filled_qty)
        fee = px * q * fee_rate
        gross = o.get("gross", 0.0) + sgn * (px - s.entry_px) * q
        fees = s.entry_fee + o.get("fees", 0.0) + fee
        exit_px = (o.get("xn", 0.0) + px * q) / s.filled_qty
        self.trader.on_closed(s.sym, exit_px, gross - fees, fees, why)

    def _take(self, s: Slot, o: dict, t: dict, px: float) -> None:
        sgn = 1 if s.plan.side == "long" else -1
        q = min(t["qty"], o["left"])
        o["left"] = round(o["left"] - q, 10)
        o["gross"] += sgn * (px - s.entry_px) * q
        o["fees"] += px * q * FEE["maker"]
        o["xn"] += px * q
        o["takes"].remove(t)
        self.trader.on_part(s.sym, t["name"], q, px)

    def on_trade(self, sym: str, tr: Trade) -> None:
        o = self.o.get(sym)
        s = self.trader.slots.get(sym) if self.trader else None
        if o is None or s is None:
            return
        long = s.plan.side == "long"
        b = self._book(sym)
        if o["kind"] == "entry" and s.state == "entry":
            if tr.ts < o["live_at"]:                          # the order has not reached the exchange yet
                return
            e = o["tick"]
            through = tr.tick < e if long else tr.tick > e
            if tr.tick == e and tr.buyer_maker == long:       # the aggressor hit our side at our price
                o["traded"] += tr.qty
            if through or o["traded"] > o["queue"] + s.qty:
                del self.o[sym]
                px = b.p(e)
                self.trader.on_entry_result(sym, s.qty, px, px * s.qty * FEE["maker"])
            return
        if o["kind"] == "open" and s.state in ("open", "closing"):
            if tr.tick <= o["stop"] if long else tr.tick >= o["stop"]:
                px = b.p(tr.tick) * (1 - 0.0002 if long else 1 + 0.0002)
                self._finish(s, px, FEE["taker"], stop_reason(s))
                return
            for t in list(o["takes"]):
                through = tr.tick > t["tick"] if long else tr.tick < t["tick"]
                if tr.tick == t["tick"] and tr.buyer_maker != long:
                    t["traded"] += tr.qty
                if through or t["traded"] > t["queue"] + t["qty"]:
                    self._take(s, o, t, b.p(t["tick"]))
            if o["left"] <= 1e-12:
                self.o.pop(sym, None)
                fees = s.entry_fee + o["fees"]
                self.trader.on_closed(sym, o["xn"] / s.filled_qty, o["gross"] - fees, fees, "take")
