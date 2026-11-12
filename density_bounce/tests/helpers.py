"""Synthetic order books for the tests: a price path (mid samples) and one density."""
from __future__ import annotations

from bounce.book import Density, SymbolBook

TICK = 0.0001

# The tuned values the tests depend on, pinned explicitly (the defaults of counter_accel / mkt_max may change after a
# study; no test may rely on them). Fixtures apply this on top of setup.P; tests that exercise one value set it again.
PIN_P = {"min_age": 60.0, "shadow_age": 20.0, "hold_min": 60.0, "hold_cancel": 30.0, "mkt_max": 0.01,
         "mkt_secs": 1800.0, "counter_accel": 1.0, "confirm_secs": 0.0, "intact": 0.7, "stop_atr": 2.0,
         "atr_n": 14, "trend_ema": 20, "cancel_after": 180.0}
PIN_RISK = {"day_loss_r": 6.0, "guard_n": 30, "guard_r": -0.3, "wall_cool": 1800.0, "sym_cooldown": 60.0}


def make_book(path: list[tuple[float, float]], dens_price: float, side: str = "bid", now: float = 1000.0,
              usd: float = 100_000.0, age: float = 100.0, sym: str = "XUSDT", turnover: float = 500_000.0,
              depth: float = 1000.0, bars: bool = True, atr: float = 0.0002, n_bars: int = 16) -> SymbolBook:
    """`path` = [(seconds before now, mid)], oldest first; the last mid is the current one. `depth` = qty every 5
    ticks up to 1 % beyond the density (what a stop behind it fills against). `bars` seeds `n_bars` complete 1m bars
    of range `atr` centred on the last mid (so the ATR exists, 2 x ATR stays below the 0.15 % stop floor for the
    default range, and they never become the extreme of the move); bars=False leaves the book without bars."""
    b = SymbolBook(sym, TICK)
    for ago, mid in path:
        b.mids.append((now - ago, mid))
    mid = path[-1][1]
    if bars:
        cur = int(now // 60)
        for m in range(cur - n_bars, cur):
            b.bars.append([m, mid + atr / 2, mid - atr / 2])
    m = b.t(mid)
    b.bids = {m - 1: 100.0, m - 2: 100.0}
    b.asks = {m + 1: 100.0, m + 2: 100.0}
    k = b.t(dens_price)
    sgn = 1 if side == "bid" else -1
    behind = b.bids if side == "bid" else b.asks
    for i in range(5, 101, 5):
        behind[k - sgn * i] = depth
    behind[k] = usd / dens_price
    b.densities[(side, k)] = Density(side, k, b.p(k), now - age, usd, usd, now)
    b.turn_q.append((now, turnover))                     # one print = this USDT per minute of turnover
    b.turn_sum = turnover
    b.synced, b.last_ts = True, now
    return b


def long_book(now: float = 1000.0, **kw) -> SymbolBook:
    """A drop from 1.0400 (25 min ago) through 1.0140 (3 min ago) and 1.0070 (60 s ago) to 1.0030, onto a bid
    density at 1.0000."""
    return make_book([(1500, 1.0400), (180, 1.0140), (60, 1.0070), (0, 1.0030)], 1.0000, "bid", now, **kw)


def short_book(now: float = 1000.0, **kw) -> SymbolBook:
    """The mirror: a rally from 0.9600 through 0.9860 and 0.9930 to 0.9970, into an ask density at 1.0000."""
    return make_book([(1500, 0.9600), (180, 0.9860), (60, 0.9930), (0, 0.9970)], 1.0000, "ask", now, **kw)
