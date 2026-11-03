"""keys.from_control against a local HTTP server playing control's keyserver."""
import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from carry.keys import from_control


@pytest.fixture()
def control(monkeypatch):
    state = {"code": 200, "hits": 0}

    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_GET(self):
            state["hits"] += 1
            ok = self.path == "/v1/binance-key" and self.headers.get("X-Control-Token") == "tok"
            code = state["code"] if ok else 401
            body = {"api_key": "K", "api_secret": "S", "chat_id": 1} if code == 200 else {"error": "nope"}
            raw = json.dumps(body).encode()
            self.send_response(code)
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

    srv = HTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    for v in ("BINANCE_API_KEY", "BINANCE_API_SECRET"):
        monkeypatch.delenv(v, raising=False)
    monkeypatch.setenv("KEY_FROM_CONTROL", f"http://127.0.0.1:{srv.server_address[1]}/")
    monkeypatch.setenv("CONTROL_TOKEN", "tok")
    yield state
    srv.shutdown()


def test_key_lands_in_the_environment(control, monkeypatch):
    import os
    assert from_control(tries=1) == ""
    assert os.environ["BINANCE_API_KEY"] == "K" and os.environ["BINANCE_API_SECRET"] == "S"


def test_refusals_are_reported_without_retrying(control, monkeypatch):
    monkeypatch.setenv("CONTROL_TOKEN", "wrong")
    assert "401" in from_control(tries=5, wait=0)
    assert control["hits"] == 1
    monkeypatch.setenv("CONTROL_TOKEN", "tok")
    control["code"] = 404
    assert "404" in from_control(tries=5, wait=0)


def test_unreachable_control_is_retried_then_reported(monkeypatch):
    monkeypatch.delenv("BINANCE_API_KEY", raising=False)
    monkeypatch.setenv("KEY_FROM_CONTROL", "http://127.0.0.1:9")      # nothing listens there
    monkeypatch.setenv("CONTROL_TOKEN", "tok")
    assert "not reachable" in from_control(tries=2, wait=0)


def test_off_or_overridden(monkeypatch):
    monkeypatch.delenv("KEY_FROM_CONTROL", raising=False)
    assert from_control() == ""
    monkeypatch.setenv("KEY_FROM_CONTROL", "http://127.0.0.1:9")
    monkeypatch.setenv("BINANCE_API_KEY", "set-on-the-runner")
    assert from_control() == ""                                          # an explicit key wins
