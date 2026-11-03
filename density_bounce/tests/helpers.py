"""Synthetic order books for the tests: a price path (mid samples) and one density."""
from __future__ import annotations

from bounce.book import Density, SymbolBook

TICK = 0.0001


def make_book(path: list[tuple[float, float]], dens_price: float, side: str = "bid", now: float = 1000.0,
              usd: float = 100_000.0, age: float = 100.0, sym: str = "XUSDT") -> SymbolBook:
    """`path` = [(seconds before now, mid)], oldest first; the last mid is the current one."""
    b = SymbolBook(sym, TICK)
    for ago, mid in path:
        b.mids.append((now - ago, mid))
    mid = path[-1][1]
    m = b.t(mid)
    b.bids = {m - 1: 100.0, m - 2: 100.0}
    b.asks = {m + 1: 100.0, m + 2: 100.0}
    k = b.t(dens_price)
    (b.bids if side == "bid" else b.asks)[k] = usd / dens_price
    b.densities[(side, k)] = Density(side, k, b.p(k), now - age, usd, usd, now)
    b.synced, b.last_ts = True, now
    return b


def long_book(now: float = 1000.0, **kw) -> SymbolBook:
    """A drop from 1.0100 (200 s ago) through 1.0070 (60 s ago) to 1.0030, onto a bid density at 1.0000."""
    return make_book([(200, 1.0100), (60, 1.0070), (0, 1.0030)], 1.0000, "bid", now, **kw)


def short_book(now: float = 1000.0, **kw) -> SymbolBook:
    """The mirror: a rally from 0.9900 through 0.9930 to 0.9970, into an ask density at 1.0000."""
    return make_book([(200, 0.9900), (60, 0.9930), (0, 0.9970)], 1.0000, "ask", now, **kw)
