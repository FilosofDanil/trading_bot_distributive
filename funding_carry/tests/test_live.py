import threading

import pytest

from carry import strategy
from carry.live import ExchangeError, Fill, Live, Perp, Protect, base_asset
from carry.runner import Runner
from carry.state import Store

SYM = "XUSDT"


class FakeExchange:
    """Balances, an isolated short per symbol, exchange stops; `fail` names methods that raise once."""

    def __init__(self, usdt=1000.0, price=1.0):
        self.px = {SYM: [price, price], "YUSDT": [2.0, 2.0]}
        self.spot = {"USDT": usdt}
        self.fut = 0.0                       # futures available balance
        self.pos: dict[str, Perp] = {}
        self.stop_orders: dict[str, list[tuple[str, float]]] = {}
        self.fail: set[str] = set()
        self.calls: list[str] = []
        self.n = 0

    def _check(self, name):
        self.calls.append(name)
        if name in self.fail:
            self.fail.discard(name)
            raise ExchangeError(f"{name} failed (test)")

    def prices(self, sym):
        return tuple(self.px[sym])

    def filters(self, sym):
        return 0.001, 5.0, 0.001, 5.0

    def spot_free(self, asset):
        return self.spot.get(asset, 0.0)

    def futures_available(self):
        return self.fut

    def transfer(self, amount, to_futures):
        self._check("transfer")
        src, dst = ("spot", "fut") if to_futures else ("fut", "spot")
        if (self.spot["USDT"] if src == "spot" else self.fut) < amount - 1e-9:
            raise ExchangeError("insufficient balance")
        if to_futures:
            self.spot["USDT"] -= amount
            self.fut += amount
        else:
            self.fut -= amount
            self.spot["USDT"] += amount

    def prepare_short(self, sym):
        self._check("prepare_short")

    def perp_order(self, sym, side, qty, reduce):
        self._check("perp_order")
        px = self.px[sym][0]
        p = self.pos.get(sym)
        if side == "SELL":
            if self.fut < qty * px:
                raise ExchangeError("margin insufficient")
            self.fut -= qty * px * 1.0005
            if p is None:
                self.pos[sym] = Perp(-qty, px, px, 0.0, qty * px)
            else:
                p.amt -= qty
                p.margin += qty * px
        else:
            assert p is not None and reduce
            share = qty / abs(p.amt)
            self.fut += p.margin * share + (p.entry - px) * qty - qty * px * 0.0005
            p.margin *= 1 - share
            p.amt += qty
            if abs(p.amt) < 1e-12:
                del self.pos[sym]
        self._liq(sym)
        return Fill(qty, px, qty * px * 0.0005)

    def _liq(self, sym):
        p = self.pos.get(sym)
        if p:   # short: liquidated when the loss eats the margin
            p.liq = p.entry + p.margin / abs(p.amt)
            p.mark = self.px[sym][0]

    def spot_order(self, sym, side, qty):
        self._check("spot_order")
        px, base = self.px[sym][1], base_asset(sym)
        if side == "BUY":
            if self.spot["USDT"] < qty * px:
                raise ExchangeError("insufficient balance")
            self.spot["USDT"] -= qty * px
            self.spot[base] = self.spot.get(base, 0.0) + qty * 0.999
            return Fill(qty * 0.999, px, qty * px * 0.001)
        self.spot[base] -= qty
        self.spot["USDT"] += qty * px * 0.999
        return Fill(qty, px, qty * px * 0.001)

    def add_margin(self, sym, amount):
        self._check("add_margin")
        if self.fut < amount - 1e-9:
            raise ExchangeError("-4050")
        self.fut -= amount
        self.pos[sym].margin += amount
        self._liq(sym)

    def positions(self):
        for s in self.pos:
            self._liq(s)
        return dict(self.pos)

    def stops(self, syms):
        return {s: [i for i, _ in self.stop_orders[s]] for s in syms if self.stop_orders.get(s)}

    def place_stop(self, sym, trigger):
        self._check("place_stop")
        self.n += 1
        self.stop_orders.setdefault(sym, []).append((str(self.n), trigger))
        return str(self.n)

    def cancel_stops(self, sym):
        self.stop_orders.pop(sym, None)

    def funding(self, sym, since_ms):
        return 0.0


@pytest.fixture()
def live(tmp_path):
    ex = FakeExchange()
    store = Store(tmp_path / "live.json", 500.0)
    return Live(ex, store, Protect()), ex, store


def test_open_pair_hedges_margins_and_places_the_stop(live):
    lv, ex, store = live
    h = lv.open_pair(SYM, 100.0, "test")
    assert h is not None and h["q"] == pytest.approx(100.0)
    assert ex.pos[SYM].amt == pytest.approx(-100.0)
    assert ex.spot["X"] >= h["q"] * 0.999 and h["q_spot"] == pytest.approx(ex.spot["X"])   # fee in the coin
    assert ex.pos[SYM].margin == pytest.approx(strategy.P["buffer"] * 100.0, rel=0.02)     # total 2N
    assert ex.pos[SYM].liq > 2.9                                                           # ~ +195 %
    (_, trigger), = ex.stop_orders[SYM]
    assert trigger == pytest.approx(2.2)                                                   # +120 %
    assert SYM in store.snapshot()["held"]


def test_spot_failure_unwinds_the_short(live):
    lv, ex, store = live
    ex.fail.add("spot_order")
    assert lv.open_pair(SYM, 100.0, "test") is None
    assert SYM not in ex.pos and ex.spot["USDT"] == pytest.approx(1000.0, abs=0.2) and ex.fut < 1
    assert not store.snapshot()["held"]
    assert any(e["level"] == "error" and "unwound" in e["msg"] for e in store.snapshot()["events"])


def test_perp_failure_returns_the_margin(live):
    lv, ex, store = live
    ex.fail.add("perp_order")
    assert lv.open_pair(SYM, 100.0, "test") is None
    assert ex.spot["USDT"] == pytest.approx(1000.0) and ex.fut == pytest.approx(0.0)


def test_refuses_existing_position_and_missing_cash(live):
    lv, ex, _ = live
    ex.pos[SYM] = Perp(0.5, 1.0, 1.0, 0.0, 1.0)            # the owner's own long
    assert lv.open_pair(SYM, 100.0, "test") is None
    del ex.pos[SYM]
    ex.spot["USDT"] = 50.0
    assert lv.open_pair(SYM, 100.0, "test") is None
    assert "perp_order" not in ex.calls


def test_close_pair_unwinds_both_legs_and_books_pnl(live):
    lv, ex, store = live
    h = lv.open_pair(SYM, 100.0, "test")
    ex.px[SYM] = [1.1, 1.1]                                 # +10 %: short -10, spot +10
    rec = lv.close_pair(SYM, h, "test close")
    assert rec is not None and SYM not in ex.pos and not ex.stop_orders.get(SYM)
    assert ex.spot.get("X", 0) < 0.2                        # only dust left
    assert abs(rec["pnl"]) < 1.5                            # hedged: fees + rounding only
    st = store.snapshot()
    assert not st["held"] and st["capital"] == pytest.approx(500.0 + rec["pnl"])
    assert ex.fut < 1.0                                     # margin went back to spot


def test_watchdog_adds_margin_when_liquidation_nears(live):
    lv, ex, store = live
    lv.open_pair(SYM, 100.0, "test")
    ex.pos[SYM].margin = 160.0                              # liq at 2.6
    ex.px[SYM] = [1.75, 1.75]                               # liq distance ~49 % < 60 %, rise 75 % < 90 %
    before = ex.pos[SYM].margin
    lv.watch_once()
    assert ex.pos[SYM].margin > before
    assert any("added" in e["msg"] for e in store.snapshot()["events"])
    lv.watch_once()                                         # rate limited: no second top-up at once
    assert ex.calls.count("add_margin") == 2               # one at open, one top-up


def test_watchdog_closes_before_liquidation(live):
    lv, ex, store = live
    lv.open_pair(SYM, 100.0, "test")
    ex.px[SYM] = [1.95, 1.95]                               # +95 % >= close_rise
    lv.watch_once()
    st = store.snapshot()
    assert SYM not in ex.pos and not st["held"] and SYM in st["cool"]
    assert any("liquidation protection" in e["msg"] for e in st["events"])


def test_watchdog_replaces_a_missing_stop(live):
    lv, ex, _ = live
    lv.open_pair(SYM, 100.0, "test")
    ex.stop_orders.clear()
    lv.watch_once()
    assert len(ex.stop_orders[SYM]) == 1


def test_watchdog_sells_spot_when_the_perp_leg_is_gone(live):
    lv, ex, store = live
    lv.open_pair(SYM, 100.0, "test")
    ex.fut += ex.pos.pop(SYM).margin                        # the exchange stop fired
    lv.watch_once()
    st = store.snapshot()
    assert not st["held"] and ex.spot.get("X", 0) < 0.2 and SYM in st["cool"]


def test_watchdog_waits_for_the_daily_cycle(live):
    lv, ex, _ = live
    lv.open_pair(SYM, 100.0, "test")
    ex.px[SYM] = [1.95, 1.95]
    done = threading.Event()
    with lv.lock:
        t = threading.Thread(target=lambda: (lv.watch_once(), done.set()))
        t.start()
        t.join(2)
        assert done.is_set() and SYM in ex.pos             # skipped: the lock is held
    lv.watch_once()
    assert SYM not in ex.pos


def test_execute_closes_then_opens(live):
    lv, ex, store = live
    lv.open_pair(SYM, 100.0, "test")
    plan = {"close": [SYM], "pair_stopped": [], "skipped": {}, "rehedge": [], "resize": {},
            "open": ["YUSDT"], "notional": {"YUSDT": 80.0}, "score": {"YUSDT": 0.0003}}
    lv.execute(plan)
    held = store.snapshot()["held"]
    assert list(held) == ["YUSDT"] and SYM not in ex.pos and ex.pos["YUSDT"].amt == pytest.approx(-40.0)


def test_runner_flatten_pauses_and_closes(tmp_path):
    ex = FakeExchange()
    store = Store(tmp_path / "live.json", 500.0)
    lv = Live(ex, store, Protect())
    r = Runner(store, mode="live", live=lv)
    lv.open_pair(SYM, 100.0, "test")
    assert r.strategy()["mode"] == "live" and r.strategy()["open_positions"] == 1
    r.flatten(by=1)
    for _ in range(50):
        if not store.snapshot()["held"]:
            break
        threading.Event().wait(0.05)
    st = store.snapshot()
    assert not st["held"] and st["desired"] == "paused" and SYM not in ex.pos


def test_runner_live_day_opens_for_real_and_marks(tmp_path, monkeypatch):
    from carry import runner as runner_mod

    def decide(day, held, equity, cands, market_data=None, cool=None):
        opens = [s for s in cands if s not in held]
        return {"day": day, "equity": equity, "book": opens + list(held), "notional": {s: 100.0 for s in opens},
                "open": opens, "close": [], "resize": {}, "rehedge": [], "pair_stopped": [], "skipped": {},
                "top": [(s, 0.0003) for s in cands], "score": {s: 0.0003 for s in cands}, "prices": {}}
    monkeypatch.setattr(runner_mod.universe, "update", lambda tags, day, log=None: tags)
    monkeypatch.setattr(runner_mod.universe, "candidates", lambda tags, day, spot: [SYM])
    monkeypatch.setattr(runner_mod.market, "spot_symbols", lambda: set())
    monkeypatch.setattr(runner_mod.strategy, "decide", decide)
    monkeypatch.setattr(runner_mod.strategy, "orders", lambda plan: [])
    ex = FakeExchange()
    store = Store(tmp_path / "live.json", 500.0)
    r = Runner(store, mode="live", live=Live(ex, store, Protect()))
    r.run_day(1_760_054_400_000)
    st = store.snapshot()
    assert ex.pos[SYM].amt == pytest.approx(-100.0) and SYM in st["held"]
    assert st["last_run_day"] == "2025-10-10" and st["daily"][-1]["book"] == [SYM]
    assert any("(live)" in e["msg"] for e in st["events"])
    assert r.strategy()["equity"] < 500.0                  # opening fees are in the open PnL


def test_paper_runner_refuses_flatten(tmp_path):
    r = Runner(Store(tmp_path / "p.json", 500.0))
    with pytest.raises(ValueError):
        r.flatten()
