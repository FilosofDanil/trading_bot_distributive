"""python -m carry [serve]                       control API on :PORT + the daily paper cycle (Railway entry point)
python -m carry once [--day YYYY-MM-DD] [--preview]   one cycle now against STATE_FILE (local debugging)

Environment: CONTROL_TOKEN (required to serve /v1/*), PORT (8080), STATE_FILE (/data/carry_state.json),
CARRY_CAPITAL (capital in USDT, 500; used only when the state file is created), RUN_AFTER_MIN (5),
MODE (paper | live). Live also reads BINANCE_API_KEY + BINANCE_API_SECRET or BINANCE_PRIVATE_KEY (Ed25519 PEM),
MAX_COINS (3), STATE_FILE (/data/carry_live_state.json), LIQ_TOPUP_DIST / LIQ_CLOSE_DIST / LIQ_CLOSE_RISE /
LIQ_STOP_RISE (carry/live.py Protect).
"""
from __future__ import annotations

import calendar
import os
import signal
import sys
import threading
import time

from . import STRATEGY_ID, VERSION
from .api import env_token, make_server
from .market import DAY_MS
from .runner import Runner
from .state import Store


def _runner() -> Runner:
    mode = os.environ.get("MODE", "paper").strip().lower()
    if mode not in ("paper", "live"):
        raise SystemExit(f"MODE={mode!r}: use paper or live")
    capital = float(os.environ.get("CARRY_CAPITAL", "500"))
    run_after = int(os.environ.get("RUN_AFTER_MIN", "5"))
    if mode == "paper":
        return Runner(Store(os.environ.get("STATE_FILE", "/data/carry_state.json"), capital), run_after, mode)

    # live: its own state file (the paper record stays as it was), at most MAX_COINS pairs
    from . import strategy
    from .exchange import Binance
    from .live import Live, Protect
    strategy.P["k"] = int(os.environ.get("MAX_COINS", "3"))
    store = Store(os.environ.get("STATE_FILE", "/data/carry_live_state.json"), capital)
    ex = Binance.from_env()
    try:
        missing = ex.check_permissions()
    except Exception as exc:                              # noqa: BLE001
        missing = [f"cannot read the key's permissions ({type(exc).__name__}: {exc})"]
    if missing:
        store.event("error", f"{STRATEGY_ID}: live trading is OFF - the Binance key lacks: {', '.join(missing)}. "
                             f"An HMAC key without an IP whitelist is read-only; use a self-generated Ed25519 key "
                             f"(BINANCE_PRIVATE_KEY) or an IP-restricted key on a static IP.", True)
        return Runner(store, run_after, "live-blocked")
    live = Live(ex, store, Protect.from_env())
    live.reconcile()
    return Runner(store, run_after, "live", live)


def serve() -> None:
    runner = _runner()
    token = env_token()
    port = int(os.environ.get("PORT", "8080"))
    if not token:
        print("WARNING: CONTROL_TOKEN is empty - /v1/* answers 503 until it is set", flush=True)
    try:
        srv = make_server(runner, token, port)        # "::" = Railway private network (IPv6) + IPv4
    except OSError as exc:
        print(f"WARNING: cannot bind [::]:{port} ({exc}), falling back to 0.0.0.0", flush=True)
        srv = make_server(runner, token, port, host="0.0.0.0")
    stop = threading.Event()

    def shutdown(*_):
        stop.set()
        threading.Thread(target=srv.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    st = runner.store.snapshot()
    runner.store.event("info", f"{STRATEGY_ID} v{VERSION} started: mode {runner.mode}, equity {st['capital']:.2f} USDT, "
                               f"{len(st['held'])} pair(s), {st['desired']}", True)
    threading.Thread(target=runner.loop, args=(stop,), daemon=True).start()
    if runner.live is not None:
        threading.Thread(target=runner.watch, args=(stop,), daemon=True).start()
        p = runner.live.p
        runner.store.event("info", f"{STRATEGY_ID}: LIVE with liquidation guard every 30 s: add margin when the "
                                   f"liquidation is < {p.topup_dist:.0%} away, close the pair at perp +{p.close_rise:.0%} "
                                   f"or liquidation < {p.close_dist:.0%} away, exchange stop at +{p.stop_rise:.0%}", True)
    print(f"funding-carry: API on [::]:{port}, state {runner.store.path}", flush=True)
    srv.serve_forever()
    # wait for a running cycle so the state is never left half-written (it is saved atomically anyway)
    deadline = time.time() + 25
    while runner.busy and time.time() < deadline:
        time.sleep(0.5)
    runner.store.save()
    print("funding-carry: stopped", flush=True)


def once(a: list[str]) -> None:
    runner = _runner()
    now = int(time.time() * 1000)
    day = now - now % DAY_MS
    if "--day" in a:
        day = calendar.timegm(time.strptime(a[a.index("--day") + 1], "%Y-%m-%d")) * 1000
    plan = runner.run_day(day, preview="--preview" in a)
    print({k: plan[k] for k in ("day", "book", "open", "close", "pair_stopped", "top", "candidates", "idle")})


def main() -> None:
    a = sys.argv[1:]
    cmd = a[0] if a else "serve"
    if cmd == "serve":
        serve()
    elif cmd == "once":
        once(a[1:])
    else:
        raise SystemExit(__doc__)


if __name__ == "__main__":
    main()
