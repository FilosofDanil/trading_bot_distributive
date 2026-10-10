"""Binance public market data over REST, ported from Strategy_tester/lab/data.py.

The lab caches everything on disk; a runner only needs today's data, so exchange info is cached in memory
for one UTC day and everything else is fetched when asked.
"""
from __future__ import annotations

import os
import threading
import time

import httpx
import pandas as pd

FAPI = os.environ.get("BINANCE_FAPI_URL", "https://fapi.binance.com").rstrip("/")
SPOT = os.environ.get("BINANCE_SPOT_URL", "https://api.binance.com").rstrip("/")
DAY_MS = 86_400_000
TF_MS = {"1h": 3_600_000, "1d": DAY_MS}
KCOLS = ["open_time", "open", "high", "low", "close", "volume", "close_time",
         "quote_volume", "trades", "taker_buy"]

_http = httpx.Client(timeout=30)
_daily: dict[str, tuple[int, object]] = {}
_lock = threading.Lock()


class RestrictedLocation(RuntimeError):
    """HTTP 451: Binance refuses this server's region (US IPs). Retrying does not help."""


def get(path: str, params: dict | None = None, base: str = FAPI):
    """GET with retry and a gentle brake on the 1-minute request weight."""
    for attempt in range(6):
        try:
            r = _http.get(base + path, params=params)
        except httpx.TransportError:
            time.sleep(2 * (attempt + 1))
            continue
        if r.status_code == 451:
            raise RestrictedLocation(f"GET {path}: 451 restricted location (deploy the service in an EU region)")
        if r.status_code in (418, 429) or r.status_code >= 500:
            time.sleep(10 * (attempt + 1))
            continue
        r.raise_for_status()
        w = r.headers.get("X-MBX-USED-WEIGHT-1M")
        if w and w.isdigit() and int(w) > 1800:
            time.sleep(15)
        return r.json()
    raise RuntimeError(f"GET {path} failed after retries")


def _today(name: str, fetch):
    day = int(time.time() * 1000) // DAY_MS
    with _lock:
        hit = _daily.get(name)
        if hit and hit[0] == day:
            return hit[1]
    value = fetch()
    with _lock:
        _daily[name] = (day, value)
    return value


def perpetuals() -> list[dict]:
    """USDT-margined perpetuals currently trading (exchangeInfo symbols, with deliveryDate)."""
    info = _today("fapi_exchangeInfo", lambda: get("/fapi/v1/exchangeInfo"))
    return [s for s in info["symbols"] if s.get("contractType") == "PERPETUAL"
            and s.get("quoteAsset") == "USDT" and s.get("status") == "TRADING"]


def spot_info() -> dict:
    return _today("spot_exchangeInfo", lambda: get("/api/v3/exchangeInfo", base=SPOT))


def spot_symbols() -> set[str]:
    """USDT spot pairs currently trading."""
    return {s["symbol"] for s in spot_info()["symbols"] if s.get("quoteAsset") == "USDT" and s.get("status") == "TRADING"}


def _frame(rows: list[list]) -> pd.DataFrame:
    df = pd.DataFrame([[r[0], r[1], r[2], r[3], r[4], r[5], r[6], r[7], r[8], r[9]] for r in rows], columns=KCOLS)
    for c in ("open", "high", "low", "close", "volume", "quote_volume", "taker_buy"):
        df[c] = df[c].astype(float)
    for c in ("open_time", "close_time", "trades"):
        df[c] = df[c].astype("int64")
    return df


def fetch_klines(symbol: str, tf: str, start: int, end: int) -> pd.DataFrame:
    """All closed perp klines with open_time in [start, end)."""
    parts, cursor = [], start
    page = 1000                      # weight 5 per call (1500 costs 10): 1/3 more bars per weight unit
    while cursor < end:
        rows = get("/fapi/v1/klines", {"symbol": symbol, "interval": tf, "startTime": cursor,
                                       "endTime": end - 1, "limit": page})
        if not rows:
            break
        part = _frame(rows)
        parts.append(part)
        cursor = int(part["open_time"].iloc[-1]) + TF_MS[tf]
        if len(rows) < page:
            break
    if not parts:
        return pd.DataFrame(columns=KCOLS)
    df = pd.concat(parts, ignore_index=True).drop_duplicates("open_time")
    now = int(time.time() * 1000)
    return df[(df["open_time"] < end) & (df["close_time"] < now)].reset_index(drop=True)


def spot_daily_open(symbol: str, start: int, end: int) -> dict[int, float]:
    """Spot 1d bars in [start, end): open_time -> open (= price at 00:00 UTC)."""
    rows = get("/api/v3/klines", {"symbol": symbol, "interval": "1d", "startTime": start,
                                  "endTime": end - 1, "limit": 1000}, base=SPOT)
    return {int(r[0]): float(r[1]) for r in rows}


def funding(symbol: str, start: int, end: int) -> pd.DataFrame:
    """fundingTime, fundingRate (the lab's plain call: at most 1000 settlements from start - 1 day)."""
    rows = get("/fapi/v1/fundingRate", {"symbol": symbol, "startTime": start - DAY_MS, "endTime": end, "limit": 1000})
    df = pd.DataFrame(rows)
    if len(df):
        df["fundingTime"] = df["fundingTime"].astype("int64")
        df["fundingRate"] = df["fundingRate"].astype(float)
    return df


def open_interest(symbol: str, start: int, end: int, period: str = "1h") -> pd.DataFrame:
    """timestamp, sumOpenInterestValue (USDT); Binance keeps only the last 30 days, <= 500 rows per call."""
    out, cursor = [], max(start, int(time.time() * 1000) - 29 * DAY_MS)
    while cursor < end:
        rows = get("/futures/data/openInterestHist", {"symbol": symbol, "period": period,
                                                       "startTime": cursor, "endTime": end, "limit": 500})
        if not rows:
            break
        out += rows
        cursor = int(rows[-1]["timestamp"]) + 1
        if len(rows) < 500:
            break
    df = pd.DataFrame(out)
    if len(df):
        df["timestamp"] = df["timestamp"].astype("int64")
        for c in df.columns:
            if c not in ("symbol", "timestamp"):
                df[c] = pd.to_numeric(df[c], errors="coerce")
    return df


def ping() -> tuple[bool, int, str | None]:
    """(ok, latency ms, error) of GET /fapi/v1/time."""
    t0 = time.monotonic()
    try:
        r = _http.get(FAPI + "/fapi/v1/time", timeout=10)
        ms = int((time.monotonic() - t0) * 1000)
        if r.status_code != 200:
            return False, ms, f"HTTP {r.status_code}"
        return True, ms, None
    except httpx.HTTPError as exc:
        return False, int((time.monotonic() - t0) * 1000), str(exc)
