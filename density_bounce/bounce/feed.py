"""Market data (from the lab's lab/book/feed.py): a screener picks the symbols to watch, and each watched
symbol gets two public websockets, diff depth (local order book) and aggTrade (tape)."""
from __future__ import annotations

import asyncio
import json
import logging
import os
import random
import time

import httpx
import websockets

from . import setup
from .book import SymbolBook

log = logging.getLogger("bounce")

REST = "https://fapi.binance.com"
WS = "wss://fstream.binance.com"

SCREEN = {
    "n": 16,                    # symbols watched at once (WATCH env)
    "vol_min": 100e6,           # 24h quote volume floor (USDT; 50e6 until v0.3.0)
    "vol_max": 3e9,             # skip the giants: their densities are noise relative to the flow
    "every": 300.0,             # the tick limit is setup.P["tick_max"] (the same coins the setup may trade)
    "never": {"BTCUSDT", "ETHUSDT", "USDCUSDT", "FDUSDUSDT", "BTCDOMUSDT"},
}


def parse_info(info: dict) -> dict[str, dict]:
    out = {}
    for s in info["symbols"]:
        if s.get("contractType") != "PERPETUAL" or s.get("quoteAsset") != "USDT" or s.get("status") != "TRADING":
            continue
        f = {x["filterType"]: x for x in s["filters"]}
        out[s["symbol"]] = {
            "tick": float(f["PRICE_FILTER"]["tickSize"]),
            "step": float(f["LOT_SIZE"]["stepSize"]),
            "min_qty": float(f["LOT_SIZE"]["minQty"]),
            "min_notional": float(f.get("MIN_NOTIONAL", {}).get("notional", 5.0)),
        }
    return out


def pick(tickers: list[dict], info: dict[str, dict], keep: set[str], exclude: set[str], n: int) -> list[str]:
    """Liquid, fine-tick perps that are the most active AND moving coins of the day, plus `keep`."""
    rows = []
    for t in tickers:
        s = t["symbol"]
        if s not in info or s in SCREEN["never"] or s in exclude:
            continue
        qv, last = float(t["quoteVolume"]), float(t["lastPrice"])
        if not (SCREEN["vol_min"] <= qv <= SCREEN["vol_max"]) or last <= 0:
            continue
        if info[s]["tick"] / last > setup.P["tick_max"]:
            continue
        rows.append((s, qv, abs(float(t["priceChangePercent"]))))
    by_v = {s: i for i, (s, _, _) in enumerate(sorted(rows, key=lambda r: r[1]))}
    by_c = {s: i for i, (s, _, _) in enumerate(sorted(rows, key=lambda r: r[2]))}
    ranked = sorted(rows, key=lambda r: -(by_v[r[0]] + 1.5 * by_c[r[0]]))
    return sorted({s for s, _, _ in ranked[:n]} | keep)


class Feed:
    def __init__(self, trader, on_trade=None, n: int | None = None):
        self.trader = trader
        self.on_trade = on_trade                 # SimBroker.on_trade in paper mode
        self.n = n or int(os.environ.get("WATCH") or SCREEN["n"])
        self.http = httpx.AsyncClient(timeout=15)
        self.tasks: dict[str, asyncio.Task] = {}
        self.msgs = 0
        self.resyncs = 0
        self.inflight: set[str] = set()
        self.watching: list[str] = []
        self.screened_at = 0.0

    async def get(self, path: str, **params):
        r = await self.http.get(REST + path, params=params)
        r.raise_for_status()
        return r.json()

    async def screener(self, exclude_fn=lambda: set()) -> None:
        while True:
            try:
                info = parse_info(await self.get("/fapi/v1/exchangeInfo"))
                self.trader.info.update(info)
                tick = await self.get("/fapi/v1/ticker/24hr")
                want = pick(tick, info, self.trader.keep(), exclude_fn(), self.n)
                if want != self.watching:
                    log.info("watching %d: %s", len(want), " ".join(want))
                self.set_symbols(want)
                chg = {t["symbol"]: round(float(t["priceChangePercent"]) / 100, 4) for t in tick}
                for s, b in self.trader.books.items():
                    b.ch24 = chg.get(s)
                self.screened_at = time.time()
            except Exception as e:
                log.warning("screener: %r", e)
                self.trader.store.event("warn", f"screener: {type(e).__name__}: {e}")
            await asyncio.sleep(SCREEN["every"])

    def set_symbols(self, want: list[str]) -> None:
        keep = self.trader.keep()
        for s in list(self.tasks):
            if s not in want and s not in keep:
                self.tasks.pop(s).cancel()
                self.trader.books.pop(s, None)
        for s in want:
            if s not in self.tasks:
                b = self.trader.books[s] = SymbolBook(s, self.trader.info[s]["tick"])
                loop = asyncio.get_event_loop()
                loop.create_task(self._seed(b))
                self.tasks[s] = loop.create_task(self.run_symbol(s))
        self.watching = sorted(self.tasks)

    async def _seed(self, b: SymbolBook) -> None:
        """The last hour of 1m bars, so a move that began before the symbol was added is measured whole (and the ATR
        of the stop is known at once), and 48 hour closes for the 1h trend. Without them both only fill from the
        tape: the move looks smaller and the setup just stays out, the trend filter stays off for 20 hours."""
        for interval, limit, seed in (("1m", 60, b.seed_bars), ("1h", 48, b.seed_h1)):
            try:
                seed(await self.get("/fapi/v1/klines", symbol=b.symbol, interval=interval, limit=limit))
            except Exception as e:
                log.warning("%s klines %s: %r", b.symbol, interval, e)

    def _spawn_snapshot(self, b: SymbolBook) -> None:
        if b.symbol not in self.inflight:
            self.inflight.add(b.symbol)
            asyncio.get_event_loop().create_task(self._snapshot(b))

    async def _snapshot(self, b: SymbolBook) -> None:
        """Fetch a REST snapshot and bridge it to the buffered stream; retry until it bridges."""
        try:
            for attempt in range(20):
                await asyncio.sleep(0.5 + attempt)        # let events buffer first
                if self.trader.books.get(b.symbol) is not b:
                    return
                try:
                    snap = await self.get("/fapi/v1/depth", symbol=b.symbol, limit=1000)
                except Exception as e:
                    log.warning("%s snapshot: %r", b.symbol, e)
                    continue
                if b.synced:                               # a gap may have reset it meanwhile
                    return
                if b.load_snapshot(snap):
                    return
                self.resyncs += 1
        finally:
            self.inflight.discard(b.symbol)

    async def run_symbol(self, sym: str) -> None:
        low = sym.lower()
        # since 2026-04-23 Binance routes depth to /public and aggTrade to /market; a wrong route is SILENT
        loop = asyncio.get_event_loop()
        depth = loop.create_task(self._loop(sym, f"{WS}/public/ws/{low}@depth@100ms", "depth"))
        trades = loop.create_task(self._loop(sym, f"{WS}/market/ws/{low}@aggTrade", "trade"))
        try:
            await asyncio.gather(depth, trades)
        except asyncio.CancelledError:
            depth.cancel()
            trades.cancel()

    async def _loop(self, sym: str, url: str, kind: str) -> None:
        backoff = 1.0
        quiet = 30.0 if kind == "depth" else 300.0       # no message for this long = dead / silent stream
        while True:
            b = self.trader.books.get(sym)
            if b is None:
                return
            if kind == "depth":
                b.reset()
            try:
                async with websockets.connect(url, ping_interval=20, ping_timeout=20, max_size=2**23) as ws:
                    started = False
                    while True:
                        raw = await asyncio.wait_for(ws.recv(), timeout=quiet)
                        data = json.loads(raw)
                        self.msgs += 1
                        if kind == "trade":
                            b.on_trade(data)
                            if self.on_trade:
                                self.on_trade(sym, b.trades[-1])
                        else:
                            if not started:
                                started = True
                                self._spawn_snapshot(b)
                            if not b.on_depth(data):
                                self.resyncs += 1
                                log.info("%s book gap -> resync", sym)
                                b.pending.append(data)
                                self._spawn_snapshot(b)
                        backoff = 1.0
            except asyncio.CancelledError:
                return
            except Exception as e:
                log.warning("%s %s ws: %r (reconnect in %.0fs)", sym, kind, e, backoff)
                await asyncio.sleep(backoff + random.random())
                backoff = min(backoff * 2, 60)

    def health(self) -> dict:
        now = time.time()
        books = self.trader.books
        return {"watching": len(books), "synced": sum(b.synced for b in books.values()),
                "max_lag_s": round(max((now - b.last_ts for b in books.values()), default=0), 1),
                "msgs": self.msgs, "resyncs": self.resyncs,
                "densities": sum(len(b.alive()) for b in books.values())}
