import json
import threading
import urllib.error
import urllib.request

import pytest

from bounce import STRATEGY_ID
from bounce.api import make_server
from bounce.runner import Runner
from bounce.state import Store

TOKEN = "t0ken"


class FakePing:
    def get(self):
        return True, 7, None


@pytest.fixture()
def api(tmp_path):
    runner = Runner(Store(tmp_path / "s.json", 100.0))       # loop not started: views run inline
    srv = make_server(runner, TOKEN, 0, host="127.0.0.1", pinger=FakePing())
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    base = f"http://127.0.0.1:{srv.server_address[1]}"

    def call(method, path, body=None, token=TOKEN):
        req = urllib.request.Request(base + path, method=method, data=json.dumps(body).encode() if body else None,
                                     headers={"X-Control-Token": token} if token else {})
        try:
            with urllib.request.urlopen(req, timeout=5) as r:
                return r.status, json.loads(r.read())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read())
    yield call, runner
    srv.shutdown()


def test_auth_and_health(api):
    call, _ = api
    assert call("GET", "/healthz", token=None) == (200, {"ok": True})
    assert call("GET", "/v1/health", token="wrong")[0] == 401
    code, h = call("GET", "/v1/health")
    assert code == 200 and h["runner"] == "bounce" and h["mode"] == "paper" and h["binance"]["ok"]
    assert h["feed"]["watching"] == 0


def test_strategy_book_is_empty_for_lossguard(api):
    call, _ = api
    code, ss = call("GET", "/v1/strategies")
    assert code == 200 and ss[0]["id"] == STRATEGY_ID and ss[0]["book"] == []
    assert ss[0]["equity"] == 100.0 and ss[0]["state"] == "running"


def test_pause_resume_events(api):
    call, runner = api
    code, r = call("POST", f"/v1/strategies/{STRATEGY_ID}/pause", {"by": 1, "reason": "tg"})
    assert code == 200 and r["strategy"]["desired"] == "paused"
    assert runner.store.data["desired"] == "paused"
    code, evs = call("GET", f"/v1/strategies/{STRATEGY_ID}/events?since=0")
    assert code == 200 and evs[-1]["notify"] and "paused" in evs[-1]["msg"]
    code, r = call("POST", f"/v1/strategies/{STRATEGY_ID}/resume", {"by": 1})
    assert r["strategy"]["desired"] == "running"


def test_plan_report_positions_have_text(api):
    call, runner = api
    runner.store.record_trade({"ts_close": 1000.0, "side": "long", "sym": "XUSDT", "reason": "take",
                               "gross": 0.05, "fees": 0.004, "pnl": 0.046, "r": 1.8})
    code, plan = call("GET", f"/v1/strategies/{STRATEGY_ID}/plan")
    assert code == 200 and plan["book"] == [] and any("rules:" in line for line in plan["text"])
    code, rep = call("GET", f"/v1/strategies/{STRATEGY_ID}/report")
    assert code == 200 and rep["trades"] == 1 and rep["equity"] == pytest.approx(100.046)
    assert rep["text"][0].startswith("trades 1")
    assert call("GET", f"/v1/strategies/{STRATEGY_ID}/positions") == (200, [])
    code, trades = call("GET", f"/v1/strategies/{STRATEGY_ID}/trades?n=5")
    assert code == 200 and trades[0]["reason"] == "take"


def test_flatten_needs_confirm_and_unknown_strategy(api):
    call, _ = api
    assert call("POST", f"/v1/strategies/{STRATEGY_ID}/flatten", {})[0] == 400
    assert call("POST", f"/v1/strategies/{STRATEGY_ID}/flatten", {"confirm": True})[0] == 409   # loop not running
    assert call("GET", "/v1/strategies/funding.carry/plan")[0] == 404
