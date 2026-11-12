"""Runner: the asyncio loop (feed + trader tick every 0.5 s + heartbeat) in a background thread, and the views
the control API serves. API calls that read the order books run inside the loop (`in_loop`), so the HTTP thread
never iterates a book while the feed mutates it."""
from __future__ import annotations

import asyncio
import logging
import os
import threading
import time
from concurrent.futures import TimeoutError as FutureTimeout

from . import STRATEGY_ID, STRATEGY_NAME, setup
from .feed import Feed
from .sim import SimBroker
from .state import Store, now_iso, utc_day
from .trader import RISK, Trader, risk_usd

log = logging.getLogger("bounce")
TICK = 0.5


class Runner:
    def __init__(self, store: Store, mode: str = "paper", broker=None, blocked: str = "",
                 notify_trades: str = "closes"):
        self.store = store
        self.mode = mode                               # paper | live | live-blocked
        self.broker = broker or SimBroker(lag=float(os.environ.get("SIM_LAG") or 0.3))
        self.trader = Trader(self.broker, store, notify_trades=notify_trades)
        self.trader.live = mode == "live"
        if blocked:
            self.trader.halt = f"live trading is OFF: {blocked}"
        on_trade = self.broker.on_trade if isinstance(self.broker, SimBroker) else None
        self.feed = Feed(self.trader, on_trade=on_trade)
        self.started = time.time()
        self.loop: asyncio.AbstractEventLoop | None = None
        self.thread: threading.Thread | None = None
        self.day = utc_day(time.time())
        self.feed_down_since = 0.0

    # ------------------------------------------------------------------ loop
    def start(self) -> None:
        ready = threading.Event()

        def run():
            self.loop = asyncio.new_event_loop()
            asyncio.set_event_loop(self.loop)
            ready.set()
            self.loop.run_until_complete(self._main())

        self.thread = threading.Thread(target=run, name="bounce-loop", daemon=True)
        self.thread.start()
        ready.wait(10)

    async def _main(self) -> None:
        if hasattr(self.broker, "start"):
            try:
                await self.broker.start()
            except Exception as e:
                self.trader.halt = f"live start failed: {e}"
                self.store.event("error", f"{STRATEGY_ID}: live start failed ({type(e).__name__}: {e}); no new entries", True)
        loop = asyncio.get_event_loop()
        loop.create_task(self.feed.screener(lambda: self.trader.blocked_syms))
        loop.create_task(self._heartbeat())
        while True:
            t0 = time.time()
            try:
                await self.trader.tick(t0)
            except Exception as e:                       # the loop must survive anything
                log.exception("tick: %s", e)
            await asyncio.sleep(max(0.05, TICK - (time.time() - t0)))

    async def _heartbeat(self) -> None:
        while True:
            await asyncio.sleep(60)
            now = time.time()
            self.store.save()                            # persists the plan / fill counters
            day = utc_day(now)
            if day != self.day:
                self.store.event("info", self.day_summary(self.day), True)
                self.day = day
            h = self.feed.health()
            down = h["watching"] > 0 and h["synced"] == 0
            if down and not self.feed_down_since:
                self.feed_down_since = now
            elif not down and self.feed_down_since:
                if now - self.feed_down_since > 180:
                    self.store.event("info", "market data back", True)
                self.feed_down_since = 0.0
            if self.feed_down_since and 180 < now - self.feed_down_since <= 240:
                self.store.event("error", f"no order book synced for {now - self.feed_down_since:.0f} s "
                                          f"({h['watching']} symbols)", True)

    def in_loop(self, fn, timeout: float = 5.0):
        """Run fn() inside the event loop thread and return its result."""
        if self.loop is None or not self.loop.is_running():
            return fn()

        async def call():
            return fn()

        try:
            return asyncio.run_coroutine_threadsafe(call(), self.loop).result(timeout)
        except FutureTimeout:
            raise RuntimeError("runner loop did not answer in time")

    # ------------------------------------------------------------------ commands
    def set_desired(self, desired: str, by=None, reason: str = "") -> dict:
        prev = self.store.data["desired"]
        self.store.set("desired", desired)
        if prev != desired:
            what = "paused: no new entries, open trades keep their stop and take" if desired == "paused" else "resumed"
            who = f" by {by}" if by else ""
            self.store.event("info", f"{STRATEGY_ID} {what}{who}{f' ({reason})' if reason else ''}", True)
        return self.strategy()

    def trigger(self) -> dict:
        """POST /run: report what the bot sees right now (as a Telegram message) and re-screen the symbols."""
        lines = self.in_loop(self.plan_lines)
        self.store.event("info", "\n".join([f"{STRATEGY_ID} now:"] + lines[:14]), True)
        if self.loop is not None:
            async def rescreen():
                try:
                    tick = await self.feed.get("/fapi/v1/ticker/24hr")
                    from .feed import pick
                    self.feed.set_symbols(pick(tick, self.trader.info, self.trader.keep(),
                                               self.trader.blocked_syms, self.feed.n))
                except Exception as e:
                    log.warning("rescreen: %r", e)
            asyncio.run_coroutine_threadsafe(rescreen(), self.loop)
        return {"ok": True, "started": True, "preview": True}

    def flatten(self, by=None) -> dict:
        self.set_desired("paused", by, "flatten")
        if self.loop is None:
            raise ValueError("runner loop is not running")
        fut = asyncio.run_coroutine_threadsafe(self.trader.flatten(), self.loop)
        n = fut.result(15)
        self.store.event("warn", f"{STRATEGY_ID}: flatten by {by}: {n} trade(s) cancelled / closed, paused", True)
        return {"ok": True, "started": True, "slots": n}

    # ------------------------------------------------------------------ views
    def _upnl(self) -> float:
        return sum(p["upnl"] for p in self.trader.view())

    def strategy(self) -> dict:
        d = self.store.data
        upnl = self.in_loop(self._upnl) if self.trader.slots else 0.0
        today = d["days"].get(utc_day(time.time()), {})
        with self.trader.lock:
            held = [s.sym for s in self.trader.slots.values() if s.state not in ("arm", "entry")]
            working = [s.sym for s in self.trader.slots.values() if s.state in ("arm", "entry")]
        state = d["desired"]
        if self.trader.halt:
            state = "error"
        last = d["trades"][-1]["ts_close"] if d["trades"] else None
        return {"id": STRATEGY_ID, "name": STRATEGY_NAME, "mode": self.mode, "state": state,
                "desired": d["desired"], "open_positions": len(held),
                # `book` stays empty on purpose: control's LossGuard leaves `book` symbols of live strategies out
                # of its loss measure (they are hedged carry legs); scalp positions must count.
                "book": [], "symbols": held, "working": working,
                "capital0": d["capital0"], "equity": round(d["capital0"] + d["totals"]["pnl"] + upnl, 4),
                "pnl_total": round(d["totals"]["pnl"], 4), "pnl_today": round(today.get("pnl", 0.0), 4),
                "trades_total": d["totals"]["n"], "trades_today": today.get("n", 0),
                "last_run_day": utc_day(time.time()), "last_run_at": now_iso(last) if last else None,
                "next_run_at": None, "last_error": self.trader.halt or d.get("last_error")}

    def plan_lines(self) -> list[str]:
        """What the bot sees now (runs inside the loop)."""
        now = time.time()
        h = self.feed.health()
        lines = [f"mode {self.mode} · {self.store.data['desired']} · watching {h['watching']} "
                 f"({h['synced']} synced, lag {h['max_lag_s']} s) · {h['densities']} densities"]
        why = self.trader.halt or self.trader.block(now)
        if why:
            lines.append(f"no new entries: {why}")
        p = setup.P
        lines.append(f"rules: approach >= {p['approach_min']:.2%}/{p['approach_secs']:.0f}s, "
                     f"move >= {p['move_min']:.2%}/{p['move_secs'] / 60:.0f}min, density >= {p['hold_min']:.0f} s of "
                     f"the flow and {p['min_age']:.0f} s old, market move < {p['mkt_max']:.1%}, entry "
                     f"{p['confirm_secs']:.0f} s after the touch, stop max({p['stop_buf']:.2%}, {p['stop_atr']:g} x ATR) "
                     f"behind, {p['tp1_part']:.0%} at +{p['tp1_r']:g}R, take {p['take_frac']:.0%} of the move, "
                     f"RR >= {p['min_rr']:g}")
        lines.append(f"risk: {risk_usd():g} USDT a stop, <= {RISK['notional']:g} USDT/trade x{RISK['leverage']}, "
                     f"max {RISK['max_open']} at once, day stop -{RISK['day_loss']:g} USDT or "
                     f"-{RISK['day_loss_r']:g}R, total stop -{RISK['total_loss']:g} USDT")
        st = self.store.data["stats"]
        lines.append(f"plans {st.get('plans', 0)} · armed {st.get('armed', 0)} · placed {st.get('placed', 0)} · "
                     f"filled {st.get('filled', 0)} · cancelled {st.get('cancelled', 0)} · shadow plans followed "
                     f"{len(self.trader.shadows)}")
        for v in self.trader.view():
            lines.append(f"{v['state'].upper()} {v['side']} {v['symbol']} entry {v['entry']:g} stop {v['stop']:g} "
                         f"take {v['tp']:g}" + (f" upnl {v['upnl']:+.4f}" if v["state"] not in ("arm", "entry") else ""))
        near = []
        for sym, b in self.trader.books.items():
            mid = b.mid()
            if not mid:
                continue
            for d in b.alive():
                near.append((abs(mid - d.price) / mid, sym, d, now))
        near.sort(key=lambda r: r[0])
        if near:
            lines.append("nearest densities:")
        for dist, sym, d, _ in near[:8]:
            lines.append(f"  {sym} {d.side} {d.price:g}  {d.usd / 1000:,.0f}k USDT  {dist:.2%} away  "
                         f"age {d.age(now):.0f}s  touches {d.touches}")
        return lines

    def plan(self) -> dict:
        d = self.store.data
        lines = self.in_loop(self.plan_lines)
        with self.trader.lock:
            syms = sorted(self.trader.slots)
        return {"day": utc_day(time.time()), "preview": False, "accepted": True,
                "paused": d["desired"] == "paused", "equity": round(d["capital0"] + d["totals"]["pnl"], 4),
                "book": [], "open": syms, "candidates": len(self.trader.books), "computed_at": now_iso(),
                "text": lines}

    def positions(self) -> list[dict]:
        out = []
        for v in self.in_loop(self.trader.view):
            if v["state"] in ("arm", "entry"):
                continue
            out.append({"symbol": v["symbol"], "n": v["n"], "days": 0, "pnl": v["upnl"],
                        "side": v["side"], "entry": v["entry"], "stop": v["stop"], "tp": v["tp"], "mark": v["mark"],
                        "text": f"{v['symbol']} {v['side']} {v['qty']:g} @ {v['entry']:g} stop {v['stop']:g} "
                                f"take {v['tp']:g} mark {v['mark'] or 0:g} upnl {v['upnl']:+.4f} ({v['age_s']} s)"})
        return out

    def day_summary(self, day: str) -> str:
        a = self.store.data["days"].get(day)
        if not a:
            return f"{STRATEGY_ID} {day}: no trades"
        return (f"{STRATEGY_ID} {day}: {a['n']} trades, win {a['wins'] / a['n']:.0%}, pnl {a['pnl']:+.4f} USDT "
                f"(fees {a['fees']:.4f}), avg {a['r'] / a['n']:+.2f}R")

    def report(self) -> dict:
        d = self.store.snapshot()
        t = d["totals"]
        cap = d["capital0"]
        eq, peak, dd, curve = cap, cap, 0.0, []
        for day in sorted(d["days"]):
            eq += d["days"][day]["pnl"]
            peak = max(peak, eq)
            dd = max(dd, (peak - eq) / peak * 100 if peak else 0.0)
            curve.append([day, round(eq, 4)])
        n = t["n"]
        lines = [f"trades {n} · win {t['wins'] / n:.1%} · avg {t['r'] / n:+.2f}R · pnl {t['pnl']:+.4f} USDT "
                 f"(gross {t['gross']:+.4f}, fees {t['fees']:.4f})" if n else "no closed trades yet"]
        for key in ("reasons", "sides"):
            for k, g in sorted(t[key].items(), key=lambda kv: -kv[1]["n"]):
                lines.append(f"  {k:<14} n {g['n']:>4}  win {g['wins'] / g['n']:>4.0%}  pnl {g['pnl']:+.4f}")
        for day in sorted(d["days"])[-7:]:
            a = d["days"][day]
            lines.append(f"{day}  n {a['n']:>3}  win {a['wins'] / a['n']:>4.0%}  pnl {a['pnl']:+.4f}")
        for tr in d["trades"][-5:]:
            lines.append(f"{now_iso(tr['ts_close'])[5:16]} {tr['side']:<5} {tr['sym']:<12} {tr['reason']:<8} "
                         f"{tr['pnl']:+.4f}" + (f" {tr['r']:+.2f}R" if tr["r"] is not None else ""))
        days = len(d["days"])
        return {"start": d["start_day"] or utc_day(time.time()), "days": days, "active_days": days,
                "capital0": cap, "equity": round(cap + t["pnl"], 4),
                "return_pct": round(t["pnl"] / cap * 100, 3) if cap else 0.0, "annual_pct": 0.0,
                "max_dd_pct": round(dd, 3), "funding": 0.0, "basis": round(t["gross"], 4), "fees": round(t["fees"], 4),
                "pairs_opened": n, "pairs_closed": t["wins"], "pair_stops": t["reasons"].get("stop", {}).get("n", 0),
                "trades": n, "wins": t["wins"], "curve": curve[-60:], "text": lines}

    def health_extra(self) -> dict:
        h = self.in_loop(self.feed.health)
        return {"feed": h, "universe_days": h["watching"]}
