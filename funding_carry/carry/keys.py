"""The Binance key from control (control/internal/keyserver): the owner stores it once with /start in Telegram,
and a live runner reads it at start into its own environment (memory only, never written or logged).

KEY_FROM_CONTROL=http://control.railway.internal:8090 turns it on; BINANCE_API_KEY set on the runner wins.
"""
from __future__ import annotations

import os
import time

import httpx


def from_control(tries: int = 12, wait: float = 10.0) -> str:
    """Fill BINANCE_API_KEY / BINANCE_API_SECRET from control. Returns '' on success or when not configured,
    otherwise why the key could not be read (the runner then starts live-blocked)."""
    url = os.environ.get("KEY_FROM_CONTROL", "").strip().rstrip("/")
    if not url or os.environ.get("BINANCE_API_KEY", "").strip():
        return ""
    token = os.environ.get("CONTROL_TOKEN", "").strip()
    if not token:
        return "KEY_FROM_CONTROL needs CONTROL_TOKEN"
    why = ""
    for attempt in range(tries):
        try:
            r = httpx.get(url + "/v1/binance-key", headers={"X-Control-Token": token}, timeout=10)
            if r.status_code == 200:
                body = r.json()
                os.environ["BINANCE_API_KEY"] = body["api_key"]
                os.environ["BINANCE_API_SECRET"] = body["api_secret"]
                return ""
            try:
                why = f"control answered {r.status_code}: {r.json().get('error', '')}"
            except ValueError:
                why = f"control answered {r.status_code}"
            if r.status_code in (401, 404, 409):              # retrying will not change these
                return why
        except httpx.HTTPError as exc:                         # control restarting: try again
            why = f"control not reachable ({type(exc).__name__}: {exc})"
        if attempt < tries - 1:
            time.sleep(wait)
    return why
