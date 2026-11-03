"""SimBroker: paper mode. Live market data, simulated fills, no orders sent (from the lab's lab/book/sim.py).

Fill model (conservative for a small order):
- post-only entry limit: rejected if it would cross; filled when a trade prints THROUGH the price, or when the
  volume traded AT the price after placement exceeds the queue that was resting there when we joined;
- stop (last price): triggered by the first trade at/through it, filled at that trade price - 0.02 % slippage;
- reduce-only take limit: the same queue rule as the entry;
- market close: best opposite price - 0.01 %.
Fees: maker 0.02 %, taker 0.05 %. Latency is not modelled, so paper results are an upper bound.
"""
from __future__ import annotations

from typing import Any

from .book import SymbolBook, Trade
from .trader import FEE, Slot


class SimBroker:
    def __init__(self):
        self.trader: Any = None                 # set by Trader()
        self.o: dict[str, dict] = {}            # sym -> working order state

    def _book(self, sym: str) -> SymbolBook:
        return self.trader.books[sym]

    async def place_entry(self, s: Slot) -> None:
        b = self._book(s.sym)
        long = s.plan.side == "long"
        e = b.t(s.plan.entry)
        bb, ba = b.best_bid(), b.best_ask()
        if bb is None or ba is None or (long and e >= ba) or (not long and e <= bb):
            raise RuntimeError("post-only would cross")
        self.o[s.sym] = {"kind": "entry", "tick": e, "queue": b.qty_at("bid" if long else "ask", e), "traded": 0.0}

    async def cancel_entry(self, s: Slot) -> None:
        o = self.o.get(s.sym)
        if o and o["kind"] == "entry":
            del self.o[s.sym]
        self.trader.on_entry_result(s.sym, 0, 0, 0)

    async def protect(self, s: Slot) -> None:
        b = self._book(s.sym)
        tp = b.t(s.plan.tp)
        side = "ask" if s.plan.side == "long" else "bid"
        self.o[s.sym] = {"kind": "open", "stop": b.t(s.plan.stop), "tp": tp, "queue": b.qty_at(side, tp),
                         "traded": 0.0}

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
        self.o.pop(s.sym, None)
        sgn = 1 if s.plan.side == "long" else -1
        fee = px * s.filled_qty * fee_rate
        pnl = sgn * (px - s.entry_px) * s.filled_qty - s.entry_fee - fee
        self.trader.on_closed(s.sym, px, pnl, s.entry_fee + fee, why)

    def on_trade(self, sym: str, tr: Trade) -> None:
        o = self.o.get(sym)
        s = self.trader.slots.get(sym) if self.trader else None
        if o is None or s is None:
            return
        long = s.plan.side == "long"
        b = self._book(sym)
        if o["kind"] == "entry" and s.state == "entry":
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
                self._finish(s, px, FEE["taker"], "stop_be" if s.manage and s.manage.be_done else "stop")
                return
            tp = o["tp"]
            through = tr.tick > tp if long else tr.tick < tp
            if tr.tick == tp and tr.buyer_maker != long:
                o["traded"] += tr.qty
            if through or o["traded"] > o["queue"] + s.filled_qty:
                self._finish(s, b.p(tp), FEE["maker"], "take")
