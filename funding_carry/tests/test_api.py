import json
import threading
import urllib.error
import urllib.request

import pytest

from carry import STRATEGY_ID, api
from carry.runner import Runner
from carry.state import Store

TOKEN = "t0ken"


@pytest.fixture()
def srv(tmp_path, monkeypatch):
    monkeypatch.setattr(api.market, "ping", lambda: (True, 5, None))
    runner = Runner(Store(tmp_path / "state.json", 500.0))
    s = api.make_server(runner, TOKEN, 0, host="127.0.0.1")    # docker build has no IPv6 loopback
    threading.Thread(target=s.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{s.server_address[1]}", runner
    s.shutdown()


def call(base, path, method="GET", token=TOKEN, body=None):
    req = urllib.request.Request(base + path, method=method, data=json.dumps(body).encode() if body else None)
    if token:
        req.add_header("X-Control-Token", token)
    try:
        with urllib.request.urlopen(req, timeout=5) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read())


def test_healthz_is_open_and_v1_needs_token(srv):
    base, _ = srv
    assert call(base, "/healthz", token=None) == (200, {"ok": True})
    assert call(base, "/v1/health", token=None)[0] == 401
    assert call(base, "/v1/health", token="wrong")[0] == 401
    code, h = call(base, "/v1/health")
    assert code == 200 and h["mode"] == "paper" and h["binance"]["ok"]


def test_pause_resume_persist_and_emit_events(srv):
    base, runner = srv
    code, r = call(base, f"/v1/strategies/{STRATEGY_ID}/pause", "POST", body={"by": 7, "reason": "tg"})
    assert code == 200 and r["strategy"]["desired"] == "paused" and r["strategy"]["state"] == "paused"
    assert json.loads(runner.store.path.read_text())["desired"] == "paused"
    _, evs = call(base, f"/v1/strategies/{STRATEGY_ID}/events?n=5")
    assert evs[-1]["notify"] and "paused" in evs[-1]["msg"]
    seq = evs[-1]["seq"]
    call(base, f"/v1/strategies/{STRATEGY_ID}/resume", "POST", body={"by": 7})
    _, newer = call(base, f"/v1/strategies/{STRATEGY_ID}/events?since={seq}")
    assert [e["seq"] for e in newer] == [seq + 1] and "resumed" in newer[0]["msg"]


def test_unknown_strategy_and_empty_plan(srv):
    base, _ = srv
    assert call(base, "/v1/strategies/other.x/plan")[0] == 404
    assert call(base, f"/v1/strategies/{STRATEGY_ID}/plan") == (404, {"error": "no plan yet"})
    code, rep = call(base, f"/v1/strategies/{STRATEGY_ID}/report")
    assert code == 200 and rep["days"] == 0 and rep["equity"] == 500.0
    code, lst = call(base, "/v1/strategies")
    assert code == 200 and lst[0]["id"] == STRATEGY_ID and lst[0]["mode"] == "paper"
