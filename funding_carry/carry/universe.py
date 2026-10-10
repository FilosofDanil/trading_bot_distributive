"""Daily candidate tags, ported from Strategy_tester/lab/universe.py (screens unchanged).

Carry candidates are the coins tagged on any of the last 30 days by either universe the lab's paper loop
builds: "coins in play" (gainers / losers / decorrelated / volume_in) and the liquid top-40. The runner keeps
those daily tags in its state and computes only the days it is missing (all 31 at the first start).
"""
from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor

import numpy as np
import pandas as pd

from .market import DAY_MS, fetch_klines, open_interest, perpetuals

PER_SCREEN = 5
MIN_QUOTE_VOL = 20_000_000   # SOURCES.md: boards filter on >= 20M USD 24h volume
EXCLUDE = {"BTCUSDT", "USDCUSDT", "FDUSDUSDT", "BTCDOMUSDT"}
LOOKBACK_DAYS = 8
LIQUID_N = 40
CAUSAL_DAYS = 30
PANEL_COLS = ["open_time", "open", "high", "low", "close", "quote_volume"]


def _hourly_panel(symbols: list[str], start: int, end: int) -> dict[str, pd.DataFrame]:
    def one(sym):
        try:
            return sym, fetch_klines(sym, "1h", start, end)[PANEL_COLS]
        except Exception:
            return sym, None
    with ThreadPoolExecutor(max_workers=4) as ex:
        return {s: df for s, df in ex.map(one, symbols) if df is not None and len(df)}


def screen_liquid(day: int, panel: dict[str, pd.DataFrame]) -> dict:
    """Alternative universe for conservative strategies: the LIQUID_N largest prior-24h quote volumes."""
    rows = []
    for sym, df in panel.items():
        if sym in EXCLUDE:
            continue
        d = df[df["open_time"] < day]
        if len(d) < 24 * 30:
            continue                              # at least a month of history
        rows.append({"symbol": sym, "qv24": float(d.iloc[-24:]["quote_volume"].sum()),
                     "chg24": float(d["close"].iloc[-1] / d.iloc[-24]["open"] - 1)})
    if not rows:
        return {"day": day, "tags": {}}
    t = pd.DataFrame(rows).nlargest(LIQUID_N, "qv24")
    return {"day": day, "tags": {s: ["liquid"] for s in t["symbol"].tolist()}}


def screen(day: int, panel: dict[str, pd.DataFrame], oi_fn=None) -> dict:
    """Coins in play for the UTC day starting at `day` (ms), using bars closed before it."""
    btc = panel["BTCUSDT"]
    btc = btc[btc["open_time"] < day].set_index("open_time")["close"]
    btc_ret = np.log(btc).diff().iloc[-72:]
    rows = []
    for sym, df in panel.items():
        if sym in EXCLUDE:
            continue
        d = df[df["open_time"] < day]
        if len(d) < 24 * 7 + 24:
            continue                              # too young: needs a full week of history
        last24 = d.iloc[-24:]
        qv24 = float(last24["quote_volume"].sum())
        if qv24 < MIN_QUOTE_VOL:
            continue
        prev7 = d.iloc[-24 * 8:-24]
        avg_day = float(prev7["quote_volume"].sum()) / 7
        chg = float(last24["close"].iloc[-1] / last24["open"].iloc[0] - 1)
        r = np.log(d.set_index("open_time")["close"]).diff()
        joined = pd.concat([r, btc_ret], axis=1, join="inner").dropna()
        corr = float(joined.iloc[:, 0].corr(joined.iloc[:, 1])) if len(joined) >= 48 else np.nan
        rows.append({"symbol": sym, "chg24": chg, "qv24": qv24,
                     "vol_ratio": qv24 / avg_day if avg_day > 0 else np.nan, "corr_btc": corr})
    if not rows:
        return {"day": day, "tags": {}}
    t = pd.DataFrame(rows)
    picks: dict[str, list[str]] = {}
    picks["gainers"] = t.nlargest(PER_SCREEN, "chg24")["symbol"].tolist()
    picks["losers"] = t.nsmallest(PER_SCREEN, "chg24")["symbol"].tolist()
    liquid = t.nlargest(100, "qv24")
    picks["decorrelated"] = liquid.dropna(subset=["corr_btc"]).nsmallest(PER_SCREEN, "corr_btc")["symbol"].tolist()
    flat = t[(t["chg24"].abs() <= 0.03) & (t["vol_ratio"] >= 2.0)].copy()
    if oi_fn is not None and len(flat):
        flat["oi_growth"] = [oi_fn(s, day) for s in flat["symbol"]]
        flat["score"] = flat["vol_ratio"] * (1 + flat["oi_growth"].fillna(0).clip(lower=0) * 10)
    else:
        flat["score"] = flat["vol_ratio"]
    picks["volume_in"] = flat.nlargest(PER_SCREEN, "score")["symbol"].tolist()
    tags: dict[str, list[str]] = {}
    for screen_name, syms in picks.items():
        for s in syms:
            tags.setdefault(s, []).append(screen_name)
    return {"day": day, "tags": tags}


def oi_growth_fn(start: int, end: int):
    cache: dict[str, pd.DataFrame | None] = {}

    def f(sym: str, day: int) -> float:
        if sym not in cache:
            try:
                cache[sym] = open_interest(sym, start, end)
            except Exception:
                cache[sym] = None
        df = cache[sym]
        if df is None or df.empty:
            return np.nan
        d = df[df["timestamp"] < day]
        if len(d) < 25:
            return np.nan
        return float(d["sumOpenInterestValue"].iloc[-1] / d["sumOpenInterestValue"].iloc[-25] - 1)
    return f


def missing_days(tags: dict[str, list[str]], today: int) -> list[int]:
    """Days of the causal window [today - 30 d, today] that have no tags yet."""
    return [d for d in range(today - CAUSAL_DAYS * DAY_MS, today + 1, DAY_MS) if str(d) not in tags]


def update(tags: dict[str, list[str]], today: int, log=print) -> dict[str, list[str]]:
    """Fill the missing days of the window (both universes, union of tags) and drop older days."""
    todo = missing_days(tags, today)
    out = {k: v for k, v in tags.items() if int(k) >= today - CAUSAL_DAYS * DAY_MS}
    if not todo:
        return out
    syms = [s["symbol"] for s in perpetuals()]
    log(f"universe: {len(todo)} day(s) to build over {len(syms)} perps")
    # one hourly panel covers both screens: the liquid screen needs 30 days before its day, in-play 8
    panel = _hourly_panel(syms, todo[0] - 31 * DAY_MS, todo[-1])
    if "BTCUSDT" not in panel:
        raise RuntimeError("universe: no BTCUSDT hourly bars")
    oi_fn = oi_growth_fn(todo[0] - 2 * DAY_MS, todo[-1])
    for d in todo:
        play = screen(d, panel, oi_fn)["tags"]
        liquid = screen_liquid(d, panel)["tags"]
        out[str(d)] = sorted(set(play) | set(liquid))
    return out


def candidates(tags: dict[str, list[str]], day: int, spot: set[str]) -> list[str]:
    """Coins tagged in the last 30 days up to `day` that have a USDT spot pair (= lab candidates_today)."""
    syms: set[str] = set()
    for k, v in tags.items():
        if day - CAUSAL_DAYS * DAY_MS <= int(k) <= day:
            syms.update(v)
    return sorted(s for s in syms if s in spot)
