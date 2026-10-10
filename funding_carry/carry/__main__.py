"""python -m carry [serve]                       control API on :PORT + the daily paper cycle (Railway entry point)
python -m carry once [--day YYYY-MM-DD] [--preview]   one cycle now against STATE_FILE (local debugging)

Environment: CONTROL_TOKEN (required to serve /v1/*), PORT (8080), STATE_FILE (/data/carry_state.json),
CARRY_CAPITAL (paper capital in USDT, 500; used only when the state file is created), RUN_AFTER_MIN (5),
MODE (paper; anything else is refused: live execution is not implemented on purpose).
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
    if mode != "paper":
        raise SystemExit(f"MODE={mode!r}: only paper is implemented. Live orders need the owner's explicit approval, "
                         "an IP-locked key and a paper record first (Strategy_tester reports/critique).")
    capital = float(os.environ.get("CARRY_CAPITAL", "500"))
    store = Store(os.environ.get("STATE_FILE", "/data/carry_state.json"), capital)
    return Runner(store, int(os.environ.get("RUN_AFTER_MIN", "5")), mode)


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
