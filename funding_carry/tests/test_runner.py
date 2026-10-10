from carry import runner as runner_mod
from carry.runner import Runner
from carry.state import Store

DAY = 1_760_054_400_000          # 2025-10-10 00:00 UTC


def fake_decide(calls):
    def decide(day, held, equity, cands, market_data=None, cool=None):
        calls.append(list(cands))
        opens = [s for s in cands if s not in held]
        return {"day": day, "equity": equity, "book": opens + list(held), "notional": {s: 10.0 for s in opens},
                "open": opens, "close": [], "resize": {}, "rehedge": [], "pair_stopped": [],
                "skipped": {"ZZZUSDT": "spot younger than 7 days"} if cands else {},
                "top": [(s, 0.0003) for s in cands], "prices": {}}
    return decide


def patch(monkeypatch, calls):
    monkeypatch.setattr(runner_mod.universe, "update", lambda tags, day, log=None: tags)
    monkeypatch.setattr(runner_mod.universe, "candidates", lambda tags, day, spot: ["AAAUSDT", "BBBUSDT"])
    monkeypatch.setattr(runner_mod.market, "spot_symbols", lambda: set())
    monkeypatch.setattr(runner_mod.strategy, "decide", fake_decide(calls))
    monkeypatch.setattr(runner_mod.strategy, "orders", lambda plan: [])


def test_paused_preview_keeps_top_and_would_open(tmp_path, monkeypatch):
    calls = []
    patch(monkeypatch, calls)
    r = Runner(Store(tmp_path / "s.json", 500.0))
    r.set_desired("paused")
    plan = r.run_day(DAY, preview=True)
    assert calls == [["AAAUSDT", "BBBUSDT"], []]          # full decision for info, acted-on one without new pairs
    assert plan["paused"] and plan["open"] == [] and plan["book"] == []
    assert [s for s, _ in plan["top"]] == ["AAAUSDT", "BBBUSDT"]
    assert plan["would_open"] == ["AAAUSDT", "BBBUSDT"] and plan["candidates"] == 2
    assert "ZZZUSDT" in plan["skipped"]
    ev = r.store.snapshot()["events"][-1]
    assert ev["notify"] and "a resume would open AAAUSDT, BBBUSDT" in ev["msg"] and "top funding" in ev["msg"]


def test_running_preview_opens_and_decides_once(tmp_path, monkeypatch):
    calls = []
    patch(monkeypatch, calls)
    r = Runner(Store(tmp_path / "s.json", 500.0))
    plan = r.run_day(DAY, preview=True)
    assert calls == [["AAAUSDT", "BBBUSDT"]]
    assert not plan["paused"] and plan["open"] == ["AAAUSDT", "BBBUSDT"] and plan["would_open"] == []
    assert r.store.snapshot()["plan"]["open"] == ["AAAUSDT", "BBBUSDT"]
