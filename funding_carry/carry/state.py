"""Runner state: one JSON file on the Railway volume, written atomically after every change."""
from __future__ import annotations

import copy
import json
import os
import threading
import time
from pathlib import Path

MAX_EVENTS = 300


def now_iso() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def fresh(capital: float) -> dict:
    return {"version": 1, "desired": "running", "capital0": capital, "capital": capital, "start_day": None,
            "held": {}, "cool": {}, "tags": {}, "daily": [], "days": [], "closed": [],
            "stats": {"pairs_opened": 0, "pairs_closed": 0, "pair_stops": 0},
            "plan": None, "events": [], "seq": 0, "errors": [],
            "last_run_day": None, "last_run_at": None, "last_error": None}


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
            self.save()
        print(f"[{level}] {msg}", flush=True)
        return ev

    def size(self) -> int:
        try:
            return self.path.stat().st_size
        except OSError:
            return 0
