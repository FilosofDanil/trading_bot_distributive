"""Local order book (Binance USD-M diff depth), tape and density detector for one symbol.

Prices are kept as integer ticks (round(price / tickSize)) so dict keys are exact. Everything here is pure
(no I/O): the feed calls load_snapshot / on_depth / on_trade, the setups read the derived state.
"""
from __future__ import annotations

import statistics
from collections import deque
from dataclasses import dataclass, field

# density detector parameters (pre-registered, not tuned: the forward test logs every feature to judge them later)
DENS = {
    "band": 0.015,        # look for densities within 1.5 % of mid
    "base_band": 0.02,    # baseline = median level notional within 2 % of mid on that side
    "ratio": 8.0,         # density >= 8 x the median level
    "usd_min": 25_000.0,  # and >= 25k USDT (50k in v0.2.0: the replay in the README earned less with it)
    "minute_k": 0.5,      # and >= half the average traded USDT per minute (last 15 min of tape)
    "gone_frac": 0.30,    # a density with < 30 % of its peak size left is gone
}


@dataclass
class Density:
    side: str              # "bid" (support, long setups) or "ask" (resistance, short setups)
    tick: int              # price in ticks
    price: float
    first_seen: float
    peak_usd: float
    usd: float
    last_seen: float
    touches: int = 0       # completed approaches: mid came within 0.15 % and went back out beyond 0.4 %
    near: bool = False
    gone: str = ""         # "" alive, "eaten" (traded through), "pulled" (cancelled), "crossed"
    gone_at: float = 0.0
    base_buy: float = 0.0  # cumulative aggressive qty at this price when the density was first seen
    base_sell: float = 0.0

    def age(self, now: float) -> float:
        return now - self.first_seen


@dataclass
class Trade:
    ts: float
    tick: int
    qty: float
    buyer_maker: bool      # True = aggressive seller hit the bid


@dataclass
class SymbolBook:
    symbol: str
    tick_size: float
    bids: dict[int, float] = field(default_factory=dict)   # tick -> qty
    asks: dict[int, float] = field(default_factory=dict)
    last_u: int | None = None
    synced: bool = False
    pending: list = field(default_factory=list)
    snapshot_id: int | None = None
    trades: deque = field(default_factory=lambda: deque(maxlen=50_000))
    mids: deque = field(default_factory=lambda: deque(maxlen=2_400))      # (ts, mid) every >= 0.25 s
    bars: deque = field(default_factory=lambda: deque(maxlen=240))        # [minute, high, low] of traded prices
    densities: dict[tuple[str, int], Density] = field(default_factory=dict)
    last_ts: float = 0.0
    cum_at: dict[int, list] = field(default_factory=dict)                # tick -> [buy qty, sell qty] cumulative
    turn_q: deque = field(default_factory=deque)                          # (ts, usd) of the last 15 min
    turn_sum: float = 0.0

    # ------------------------------------------------------------------ sync
    def t(self, price: str | float) -> int:
        return int(round(float(price) / self.tick_size))

    def p(self, tick: int) -> float:
        return round(tick * self.tick_size, 12)

    def reset(self) -> None:
        self.bids.clear(); self.asks.clear()
        self.last_u = None; self.synced = False; self.snapshot_id = None; self.pending = []

    def load_snapshot(self, snap: dict) -> bool:
        """Apply a REST snapshot, then replay buffered events. Returns False if the buffer does not bridge it."""
        self.bids = {self.t(p): float(q) for p, q in snap["bids"] if float(q) > 0}
        self.asks = {self.t(p): float(q) for p, q in snap["asks"] if float(q) > 0}
        self.snapshot_id = int(snap["lastUpdateId"])
        self.last_u = None
        buf, self.pending = self.pending, []
        self.synced = True
        for ev in buf:
            if not self.on_depth(ev):
                return False
        return True

    def on_depth(self, ev: dict) -> bool:
        """Apply one diff event. Returns False when the book must be resynced (gap)."""
        if not self.synced:
            self.pending.append(ev)
            return True
        U, u, pu = int(ev["U"]), int(ev["u"]), int(ev.get("pu", -1))
        if self.last_u is None:                      # first event after the snapshot
            if u < self.snapshot_id:
                return True                          # older than the snapshot: drop
            if U > self.snapshot_id:
                self.reset(); return False           # gap between snapshot and stream
        elif pu != self.last_u:
            self.reset(); return False
        for side, book in (("b", self.bids), ("a", self.asks)):
            for p, q in ev[side]:
                k, qty = self.t(p), float(q)
                if qty == 0:
                    book.pop(k, None)
                else:
                    book[k] = qty
        self.last_u = u
        self.last_ts = float(ev.get("E", 0)) / 1000
        self._sample_mid(self.last_ts)
        return True

    def on_trade(self, ev: dict) -> None:
        tr = Trade(float(ev["T"]) / 1000, self.t(ev["p"]), float(ev["q"]), bool(ev["m"]))
        self.trades.append(tr)
        self.last_ts = max(self.last_ts, tr.ts)
        cum = self.cum_at.setdefault(tr.tick, [0.0, 0.0])          # [aggressive buys, aggressive sells]
        cum[1 if tr.buyer_maker else 0] += tr.qty
        px = tr.tick * self.tick_size
        usd = tr.qty * px
        self.turn_q.append((tr.ts, usd))
        self.turn_sum += usd
        self._prune_turn(tr.ts)
        self._bar(tr.ts, px)

    def _prune_turn(self, now: float) -> None:
        while self.turn_q and self.turn_q[0][0] < now - 900:
            self.turn_sum -= self.turn_q.popleft()[1]

    def _bar(self, ts: float, px: float) -> None:
        m = int(ts // 60)
        if self.bars and self.bars[-1][0] == m:
            bar = self.bars[-1]
            bar[1], bar[2] = max(bar[1], px), min(bar[2], px)
        elif not self.bars or m > self.bars[-1][0]:
            self.bars.append([m, px, px])

    def seed_bars(self, klines: list) -> None:
        """Put 1m klines ([open ms, open, high, low, ...]) older than our own tape in front of the bars."""
        first = self.bars[0][0] if self.bars else None
        old = [[int(k[0]) // 60_000, float(k[2]), float(k[3])] for k in klines]
        self.bars.extendleft(reversed([b for b in old if first is None or b[0] < first]))

    def cum(self, tick: int, aggressor: str | None = None) -> float:
        """Cumulative qty traded at `tick` since the stream started (both sides or one aggressor)."""
        c = self.cum_at.get(tick, (0.0, 0.0))
        return c[0] if aggressor == "buy" else c[1] if aggressor == "sell" else c[0] + c[1]

    # ------------------------------------------------------------------ views
    def best_bid(self) -> int | None:
        return max(self.bids) if self.bids else None

    def best_ask(self) -> int | None:
        return min(self.asks) if self.asks else None

    def mid(self) -> float | None:
        b, a = self.best_bid(), self.best_ask()
        if b is None or a is None or b >= a:
            return None
        return (b + a) / 2 * self.tick_size

    def _sample_mid(self, ts: float) -> None:
        if self.mids and ts - self.mids[-1][0] < 0.25:
            return
        m = self.mid()
        if m:
            self.mids.append((ts, m))

    def mid_ago(self, now: float, seconds: float) -> float | None:
        """Mid price about `seconds` ago (the newest sample not newer than now - seconds)."""
        target = now - seconds
        best = None
        for ts, m in reversed(self.mids):
            if ts <= target:
                best = m
                break
        return best

    def qty_at(self, side: str, tick: int) -> float:
        return (self.bids if side == "bid" else self.asks).get(tick, 0.0)

    def traded(self, since: float, tick: int | None = None, aggressor: str | None = None) -> float:
        """Base-asset qty traded since `since` (optionally at one price / by aggressor 'buy' or 'sell')."""
        s = 0.0
        for tr in reversed(self.trades):
            if tr.ts < since:
                break
            if tick is not None and tr.tick != tick:
                continue
            if aggressor == "buy" and tr.buyer_maker:
                continue
            if aggressor == "sell" and not tr.buyer_maker:
                continue
            s += tr.qty
        return s

    def minute_turnover(self, now: float) -> float:
        """Average traded USDT per minute over the last 15 min (or over the tape we have). O(1) rolling sum."""
        self._prune_turn(now)
        if not self.turn_q:
            return 0.0
        span = max(60.0, now - self.turn_q[0][0])
        return self.turn_sum / span * 60

    # ------------------------------------------------------------------ densities
    def scan_densities(self, now: float) -> None:
        """Refresh the density table (call every ~0.5 s)."""
        mid = self.mid()
        if mid is None:
            return
        turn = self.minute_turnover(now)
        mid_t = mid / self.tick_size
        for side, book in (("bid", self.bids), ("ask", self.asks)):
            lo = mid_t * (1 - DENS["base_band"]) if side == "bid" else mid_t
            hi = mid_t if side == "bid" else mid_t * (1 + DENS["base_band"])
            lv = [(k, q * k * self.tick_size) for k, q in book.items() if lo <= k <= hi]
            if len(lv) < 15:
                continue
            base = statistics.median(u for _, u in lv)
            thr = max(DENS["ratio"] * base, DENS["usd_min"], DENS["minute_k"] * turn)
            band_lo = mid_t * (1 - DENS["band"]) if side == "bid" else mid_t
            band_hi = mid_t if side == "bid" else mid_t * (1 + DENS["band"])
            for k, usd in lv:
                if usd >= thr and band_lo <= k <= band_hi:
                    d = self.densities.get((side, k))
                    if d is None or d.gone:
                        self.densities[(side, k)] = Density(side, k, self.p(k), now, usd, usd, now,
                                                            base_buy=self.cum(k, "buy"), base_sell=self.cum(k, "sell"))
                    else:
                        d.usd, d.last_seen = usd, now
                        d.peak_usd = max(d.peak_usd, usd)
        # update every alive density: size, touches, death
        bb, ba = self.best_bid(), self.best_ask()
        for key, d in list(self.densities.items()):
            if d.gone:
                if now - d.gone_at > 600:
                    del self.densities[key]
                continue
            qty = self.qty_at(d.side, d.tick)
            d.usd = qty * d.price
            crossed = (d.side == "bid" and ba is not None and ba <= d.tick) or \
                      (d.side == "ask" and bb is not None and bb >= d.tick)
            if crossed or d.usd < DENS["gone_frac"] * d.peak_usd:
                traded_usd = self.eaten(d) * d.price
                d.gone = "crossed" if crossed else ("eaten" if traded_usd >= 0.5 * (d.peak_usd - d.usd) else "pulled")
                d.gone_at = now
                continue
            dist = abs(mid - d.price) / d.price
            if dist <= 0.0015:
                d.near = True
            elif d.near and dist >= 0.004:
                d.near = False
                d.touches += 1

    def eaten(self, d: Density, aggressor: str | None = None) -> float:
        """Qty traded at the density's price since it appeared (optionally one aggressor side). O(1)."""
        b = self.cum(d.tick, "buy") - d.base_buy
        s = self.cum(d.tick, "sell") - d.base_sell
        return b if aggressor == "buy" else s if aggressor == "sell" else b + s

    def alive(self, side: str | None = None) -> list[Density]:
        return [d for d in self.densities.values() if not d.gone and (side is None or d.side == side)]
