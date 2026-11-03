"""Runner state: one JSON file on the Railway volume, written atomically after every change.

Holds the desired state (pause / resume from Telegram), the trade journal (last MAX_TRADES closed trades),
per-day and total aggregates (kept forever, a few hundred bytes a day), the events control polls, and in live
mode the bot's open slots so leftovers can be closed after a restart.
"""
from __future__ import annotations

import copy
import json
import os
import threading
import time
from pathlib import Path

MAX_EVENTS = 300
MAX_TRADES = 1000


def now_iso(ts: float | None = None) -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(ts))


def utc_day(ts: float) -> str:
    return time.strftime("%Y-%m-%d", time.gmtime(ts))


def _agg() -> dict:
    return {"n": 0, "wins": 0, "pnl": 0.0, "gross": 0.0, "fees": 0.0, "r": 0.0}


def fresh(capital: float) -> dict:
    return {"version": 1, "desired": "running", "capital0": capital, "start_day": None,
            "trades": [], "days": {}, "totals": _agg() | {"reasons": {}, "sides": {}}, "consec_losses": 0,
            "slots": {}, "stats": {"plans": 0, "placed": 0, "filled": 0, "cancelled": 0, "rejected": 0},
            "events": [], "seq": 0, "errors": [], "last_error": None}


class Store:
    """Thread-safe holder of the state dict; `save()` persists it (tmp file + os.replace)."""

    def __init__(self, path: str | Path, capital: float):
        self.path = Path(path)
        self.lock = threading.RLock()
        if self.path.exists():
            self.data = json.loads(self.path.read_text(encoding="utf-8"))
        else:
            self.data = fresh(capital)
            self.path.parent.mkdir(parents=True, exist_ok=True)
            self.save()

    def save(self) -> None:
        with self.lock:
            tmp = self.path.with_suffix(f".{os.getpid()}.tmp")
            tmp.write_text(json.dumps(self.data, separators=(",", ":"), default=float), encoding="utf-8")
            os.replace(tmp, self.path)

    def snapshot(self) -> dict:
        with self.lock:
            return copy.deepcopy(self.data)

    def event(self, level: str, msg: str, notify: bool = False) -> dict:
        with self.lock:
            self.data["seq"] += 1
            ev = {"seq": self.data["seq"], "ts": now_iso(), "level": level, "msg": msg, "notify": notify}
            self.data["events"] = (self.data["events"] + [ev])[-MAX_EVENTS:]
            if level == "error":
                self.data["errors"] = (self.data["errors"] + [{"ts": ev["ts"], "msg": msg}])[-5:]
                self.data["last_error"] = msg
            self.save()
        print(f"[{level}] {msg}", flush=True)
        return ev

    def record_trade(self, tr: dict) -> None:
        """Append a closed trade and update the day / total aggregates."""
        with self.lock:
            d = self.data
            day = utc_day(tr["ts_close"])
            d["start_day"] = d["start_day"] or day
            d["trades"] = (d["trades"] + [tr])[-MAX_TRADES:]
            for a in (d["days"].setdefault(day, _agg()), d["totals"]):
                a["n"] += 1
                a["wins"] += tr["pnl"] > 0
                a["pnl"] = round(a["pnl"] + tr["pnl"], 6)
                a["gross"] = round(a["gross"] + tr["gross"], 6)
                a["fees"] = round(a["fees"] + tr["fees"], 6)
                a["r"] = round(a["r"] + (tr["r"] or 0.0), 4)
            t = d["totals"]
            for key, val in (("reasons", tr["reason"]), ("sides", tr["side"])):
                g = t[key].setdefault(val, {"n": 0, "pnl": 0.0, "wins": 0})
                g["n"] += 1
                g["wins"] += tr["pnl"] > 0
                g["pnl"] = round(g["pnl"] + tr["pnl"], 6)
            d["consec_losses"] = d["consec_losses"] + 1 if tr["pnl"] < 0 else 0
            self.save()

    def set(self, key: str, value) -> None:
        with self.lock:
            self.data[key] = value
            self.save()

    def size(self) -> int:
        try:
            return self.path.stat().st_size
        except OSError:
            return 0
