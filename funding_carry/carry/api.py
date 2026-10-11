"""Control API v1 (docs/runner_api.md) on the Railway private network: stdlib HTTP server bound to "::"."""
from __future__ import annotations

import hmac
import json
import os
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

from . import RUNNER, STRATEGY_ID, VERSION, market
from .runner import Busy, Runner


def _rss_mb() -> float | None:
    try:
        with open("/proc/self/status", encoding="ascii") as f:
            for line in f:
                if line.startswith("VmRSS:"):
                    return round(int(line.split()[1]) / 1024, 1)
    except OSError:
        pass
    return None


class _Ping:
    """Binance reachability, measured at most once a minute."""

    def __init__(self):
        self.at, self.value, self.lock = 0.0, (False, 0, "not checked yet"), threading.Lock()

    def get(self) -> tuple[bool, int, str | None]:
        with self.lock:
            if time.time() - self.at > 60:
                self.value, self.at = market.ping(), time.time()
            return self.value


class Server(ThreadingHTTPServer):
    address_family = socket.AF_INET6
    daemon_threads = True

    def server_bind(self):
        try:
            self.socket.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)   # IPv4 too (local runs)
        except (AttributeError, OSError):
            pass
        super().server_bind()


def make_server(runner: Runner, token: str, port: int, host: str = "::") -> Server:
    ping = _Ping()

    class Handler(BaseHTTPRequestHandler):
        server_version = f"funding-carry/{VERSION}"

        def log_message(self, fmt, *args):            # quiet: control polls every 30 s
            pass

        def _send(self, code: int, body) -> None:
            raw = json.dumps(body, default=float).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def _auth(self) -> bool:
            if not token:
                self._send(503, {"error": "CONTROL_TOKEN is not set on the runner"})
                return False
            got = self.headers.get("X-Control-Token", "")
            if not hmac.compare_digest(got.encode(), token.encode()):
                self._send(401, {"error": "unauthorized"})
                return False
            return True

        def _body(self) -> dict:
            n = int(self.headers.get("Content-Length") or 0)
            if not n:
                return {}
            try:
                body = json.loads(self.rfile.read(min(n, 65536)))
                return body if isinstance(body, dict) else {}
            except ValueError:
                return {}

        def _route(self, method: str) -> None:
            url = urlparse(self.path)
            parts = [p for p in url.path.split("/") if p]
            if method == "GET" and parts == ["healthz"]:
                return self._send(200, {"ok": True})
            if not parts or parts[0] != "v1":
                return self._send(404, {"error": "not found"})
            if not self._auth():
                return
            q = {k: v[-1] for k, v in parse_qs(url.query).items()}
            try:
                if method == "GET" and parts == ["v1", "health"]:
                    return self._send(200, self._health())
                if method == "GET" and parts == ["v1", "strategies"]:
                    return self._send(200, [runner.strategy()])
                if len(parts) < 4 or parts[1] != "strategies":
                    return self._send(404, {"error": "not found"})
                if parts[2] != STRATEGY_ID:
                    return self._send(404, {"error": f"unknown strategy {parts[2]!r}; this runner has {STRATEGY_ID}"})
                action = parts[3]
                if method == "POST" and action in ("pause", "resume"):
                    b = self._body()
                    s = runner.set_desired("paused" if action == "pause" else "running", b.get("by"), str(b.get("reason", "")))
                    return self._send(200, {"ok": True, "strategy": s})
                if method == "POST" and action == "flatten":
                    b = self._body()
                    if b.get("confirm") is not True:
                        return self._send(400, {"error": 'flatten needs {"confirm": true}'})
                    try:
                        return self._send(200, runner.flatten(b.get("by")))
                    except ValueError as exc:
                        return self._send(409, {"error": str(exc)})
                if method == "POST" and action == "run":
                    try:
                        return self._send(200, runner.trigger())
                    except Busy:
                        return self._send(409, {"error": "busy"})
                if method == "GET" and action == "plan":
                    plan = runner.store.snapshot()["plan"]
                    return self._send(200, plan) if plan else self._send(404, {"error": "no plan yet"})
                if method == "GET" and action == "positions":
                    return self._send(200, runner.positions())
                if method == "GET" and action == "report":
                    return self._send(200, runner.report())
                if method == "GET" and action == "events":
                    return self._send(200, self._events(q))
                return self._send(404, {"error": "not found"})
            except Exception as exc:
                return self._send(500, {"error": f"{type(exc).__name__}: {exc}"})

        def _health(self) -> dict:
            st = runner.store.snapshot()
            ok, ms, err = ping.get()
            return {"runner": RUNNER, "version": VERSION, "uptime_s": int(time.time() - runner.started),
                    "mode": runner.mode, "now": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                    "binance": {"ok": ok, "latency_ms": ms, "error": err}, "rss_mb": _rss_mb(),
                    "universe_days": len(st["tags"]), "state_bytes": runner.store.size(), "busy": runner.busy,
                    "errors": st["errors"][-5:]}

        def _events(self, q: dict) -> list[dict]:
            evs = runner.store.snapshot()["events"]
            try:
                n = max(1, min(int(q.get("n", 20)), 200))
            except ValueError:
                n = 20
            if "since" in q:
                try:
                    since = int(q["since"])
                except ValueError:
                    since = 0
                return [e for e in evs if e["seq"] > since][:200]
            return evs[-n:]

        def do_GET(self):
            self._route("GET")

        def do_POST(self):
            self._route("POST")

    if ":" not in host:                               # IPv4 host (tests, containers without IPv6)
        class Server4(Server):
            address_family = socket.AF_INET
        return Server4((host, port), Handler)
    return Server((host, port), Handler)


def env_token() -> str:
    return os.environ.get("CONTROL_TOKEN", "").strip()
