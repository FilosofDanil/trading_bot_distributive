"""The daily cycle: universe -> decision -> paper fills -> marks, once per UTC day after 00:05."""
from __future__ import annotations

import json
import threading
import time
import traceback

from . import STRATEGY_ID, STRATEGY_NAME, market, paper, strategy, universe
from .market import DAY_MS
from .state import Store, now_iso

RETRY_AFTER_S = 15 * 60


class Busy(RuntimeError):
    pass


def _day(ms: int) -> str:
    return time.strftime("%Y-%m-%d", time.gmtime(ms / 1000))


def _iso(ms: int) -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(ms / 1000))


def _plain(x):
    """JSON-safe copy (tuples -> lists, numpy scalars -> float)."""
    return json.loads(json.dumps(x, default=float))


class Runner:
    def __init__(self, store: Store, run_after_min: int = 5, mode: str = "paper"):
        self.store = store
        self.run_after_ms = run_after_min * 60_000
        self.mode = mode
        self.started = time.time()
        self._busy = threading.Lock()
        self._next_try = 0.0

    @property
    def busy(self) -> bool:
        return self._busy.locked()

    # ------------------------------------------------------------ API view
    def strategy(self) -> dict:
        st = self.store.snapshot()
        now = int(time.time() * 1000)
        today = now - now % DAY_MS
        if self.busy:
            state = "busy"
        elif st["last_error"]:
            state = "error"
        elif st["desired"] == "paused":
            state = "paused"
        else:
            state = "running"
        last = st["daily"][-1] if st["daily"] else None
        pnl_today = last["funding"] + last["basis"] + last["fees"] if last and last["day"] == _day(today) else 0.0
        due = today + self.run_after_ms if st["last_run_day"] != _day(today) else today + DAY_MS + self.run_after_ms
        return {"id": STRATEGY_ID, "name": STRATEGY_NAME, "mode": self.mode, "state": state, "desired": st["desired"],
                "open_positions": len(st["held"]), "book": sorted(st["held"]),
                "capital0": st["capital0"], "equity": st["capital"], "pnl_total": st["capital"] - st["capital0"],
                "pnl_today": pnl_today, "last_run_day": st["last_run_day"], "last_run_at": st["last_run_at"],
                "next_run_at": _iso(max(due, now)), "last_error": st["last_error"]}

    def set_desired(self, desired: str, by=None, reason: str = "") -> dict:
        with self.store.lock:
            before = self.store.data["desired"]
            self.store.data["desired"] = desired
            self.store.save()
        if before != desired:
            who = f" by {by}" if by else ""
            what = "paused: no new pairs, held pairs are still managed" if desired == "paused" else "resumed"
            self.store.event("info", f"{STRATEGY_ID} {what}{who}" + (f" ({reason})" if reason else ""), True)
        return self.strategy()

    def positions(self) -> list[dict]:
        st = self.store.snapshot()
        now = int(time.time() * 1000)
        out = []
        for s, h in sorted(st["held"].items()):
            opened = h.get("opened", now)
            fund, basis, fees = h.get("funding", 0.0), h.get("basis", 0.0), h.get("fees", 0.0)
            out.append({"symbol": s, "n": h["n"], "perp0": h["perp0"], "spot0": h["spot0"], "opened": _day(opened),
                        "days": int((now - opened) // DAY_MS), "funding": fund, "basis": basis, "fees": -fees,
                        "pnl": fund + basis - fees})
        return out

    def report(self) -> dict:
        st = self.store.snapshot()
        daily = st["daily"]
        eq = [st["capital0"]] + [r["equity"] for r in daily]
        peak, dd = eq[0], 0.0
        for e in eq:
            peak = max(peak, e)
            dd = max(dd, 1 - e / peak if peak > 0 else 0.0)
        days = len(daily)
        ret = st["capital"] / st["capital0"] - 1 if st["capital0"] else 0.0
        annual = ((1 + ret) ** (365 / days) - 1) if days and ret > -1 else 0.0
        return {"start": st["start_day"], "days": days, "capital0": st["capital0"], "equity": st["capital"],
                "return_pct": ret * 100, "annual_pct": annual * 100, "max_dd_pct": dd * 100,
                "funding": sum(r["funding"] for r in daily), "basis": sum(r["basis"] for r in daily),
                "fees": sum(r["fees"] for r in daily), **st["stats"],
                "active_days": sum(1 for r in daily if r["book"]),
                "curve": [[r["day"], r["equity"]] for r in daily[-60:]]}

    # ------------------------------------------------------------ the cycle
    def trigger(self) -> dict:
        """Start today's cycle now in the background; a preview if today was already accepted."""
        if self.busy:
            raise Busy("busy")
        now = int(time.time() * 1000)
        day = now - now % DAY_MS
        preview = self.store.snapshot()["last_run_day"] == _day(day)
        threading.Thread(target=self._safe_run, args=(day, preview), daemon=True).start()
        return {"ok": True, "started": True, "preview": preview}

    def _safe_run(self, day: int, preview: bool) -> None:
        try:
            self.run_day(day, preview)
        except Busy:
            pass
        except Exception as exc:                      # keep the loop alive; control alerts on last_error
            traceback.print_exc()
            with self.store.lock:
                self.store.data["last_error"] = f"{type(exc).__name__}: {exc}"[:500]
                self.store.save()
            self.store.event("error", f"run {_day(day)} failed: {type(exc).__name__}: {exc}"[:500], True)
            self._next_try = time.time() + RETRY_AFTER_S

    def run_day(self, day: int, preview: bool = False) -> dict:
        if not self._busy.acquire(blocking=False):
            raise Busy("busy")
        try:
            return self._run_day(day, preview)
        finally:
            self._busy.release()

    def _run_day(self, day: int, preview: bool) -> dict:
        st = self.store.snapshot()
        t0 = time.time()
        tags = universe.update(st["tags"], day, log=lambda m: print(m, flush=True))
        with self.store.lock:
            self.store.data["tags"] = tags
            self.store.save()
        paused = st["desired"] == "paused"
        cands = [] if paused else universe.candidates(tags, day, market.spot_symbols())
        plan = strategy.decide(day, st["held"], st["capital"], cands, cool=st.get("cool", {}))
        plan["held_n"] = {s: v["n"] for s, v in st["held"].items()}
        od = strategy.orders(plan)
        used = (1 + strategy.P["buffer"]) * sum(plan["notional"].values())
        summary = _plain({
            "day": _day(day), "preview": preview, "accepted": not preview, "paused": paused, "equity": plan["equity"],
            "book": plan["book"], "open": plan["open"], "close": plan["close"], "resize": plan["resize"],
            "pair_stopped": plan["pair_stopped"], "rehedge": plan["rehedge"], "top": plan["top"],
            "min_f8": strategy.P["min_f8"], "skipped": plan["skipped"], "candidates": len(cands),
            "orders": od, "idle": plan["equity"] - used, "computed_at": now_iso()})
        if preview:
            with self.store.lock:
                self.store.data["plan"] = summary
                self.store.save()
            self.store.event("info", f"preview {summary['day']}: book {', '.join(plan['book']) or 'empty'} "
                                     f"({len(cands)} candidates, {time.time() - t0:.0f} s)")
            return summary

        rates = {}
        for s, h in st["held"].items():
            f = market.funding(s, h.get("marked", day), day)
            rates[s] = [(int(t), float(r)) for t, r in zip(f["fundingTime"], f["fundingRate"])] if len(f) else []
        marks = paper.mark(st["held"], plan["prices"], rates, day)
        with self.store.lock:
            data = self.store.data
            data["held"] = st["held"]                 # the snapshot the marks were computed on
            res = paper.apply(data, plan, marks)
            data["plan"] = summary
            data["start_day"] = data["start_day"] or summary["day"]
            data["last_run_day"], data["last_run_at"], data["last_error"] = summary["day"], now_iso(), None
            self.store.save()
        for level, msg, notify in res["events"]:
            self.store.event(level, msg, notify)
        row = res["row"]
        top = ", ".join(f"{s} {sc * 100:.3f} %" for s, sc in plan["top"][:3]) or "none"
        self.store.event("info", f"day {row['day']}: book {', '.join(row['book']) or 'empty'}, equity {row['equity']:.2f} "
                                 f"USDT (funding {row['funding']:+.2f}, basis {row['basis']:+.2f}, fees {row['fees']:+.2f}); "
                                 f"top funding/8h {top}; {len(cands)} candidates, {time.time() - t0:.0f} s", True)
        return summary

    def loop(self, stop: threading.Event) -> None:
        while not stop.is_set():
            now = int(time.time() * 1000)
            day = now - now % DAY_MS
            st = self.store.snapshot()
            if st["last_run_day"] != _day(day) and now >= day + self.run_after_ms and time.time() >= self._next_try \
                    and not self.busy:
                self._safe_run(day, False)
            stop.wait(30)
