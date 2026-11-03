"""python -m bounce [serve]     control API on :PORT + the live market loop (Railway entry point)
python -m bounce check         live mode: read-only check of the key / account, sends no order

Environment: CONTROL_TOKEN (required to serve /v1/*), PORT (8080), MODE (paper | live),
STATE_FILE (/data/bounce_state.json, paper), LIVE_STATE_FILE (/data/bounce_live_state.json), CAPITAL (100; PAPER_CAPITAL also works:
the record's starting equity, used only when the state file is created), NOTIFY_TRADES (closes | all | none: which trade events go to Telegram), WATCH (16 symbols),
NOTIONAL (10 USDT per trade), LEVERAGE (2), MAX_OPEN (2), DAY_LOSS (1), TOTAL_LOSS (5), MAX_CONSEC_LOSSES (6),
MAX_TRADES_DAY (60), PARAMS (JSON object over bounce/setup.py P, e.g. {"take_frac": 0.5, "stop_buf": 0.002}).
Live also reads BINANCE_API_KEY + BINANCE_API_SECRET (HMAC) or BINANCE_PRIVATE_KEY (Ed25519 PEM), or
KEY_FROM_CONTROL (http://control.railway.internal:8090: the key stored in control with /start, bounce/keys.py).
"""
from __future__ import annotations

import asyncio
import logging
import os
import signal
import sys
import threading

from . import STRATEGY_ID, VERSION, setup
from .api import env_token, make_server
from .runner import Runner
from .state import Store
from .trader import RISK, load_risk


def _runner() -> Runner:
    mode = os.environ.get("MODE", "paper").strip().lower()
    if mode not in ("paper", "live"):
        raise SystemExit(f"MODE={mode!r}: use paper or live")
    setup.load_params()
    load_risk()
    capital = float(os.environ.get("CAPITAL") or os.environ.get("PAPER_CAPITAL") or "100")
    notify = os.environ.get("NOTIFY_TRADES", "closes").strip().lower()
    if mode == "paper":
        return Runner(Store(os.environ.get("STATE_FILE", "/data/bounce_state.json"), capital), "paper",
                      notify_trades=notify)

    # live: its own state file (the paper record stays as it was)
    from .keys import from_control
    from .live import LiveBroker, Rest, check
    # not STATE_FILE: the image sets it to the paper record
    store = Store(os.environ.get("LIVE_STATE_FILE", "/data/bounce_live_state.json"), capital)
    if store.data["totals"]["n"] == 0 and not store.data["slots"] and store.data["capital0"] != capital:
        store.set("capital0", capital)                   # nothing traded yet: CAPITAL may still change
    why = from_control()
    if why:
        # not notify: the reason is the strategy's last_error, which control already alerts once
        store.event("error", f"{STRATEGY_ID}: live trading is OFF - no Binance key from control: {why}")
        return Runner(store, "live-blocked", blocked=why, notify_trades=notify)
    rest = Rest.from_env()
    try:
        problems = asyncio.run(check(rest))
    except Exception as exc:                             # noqa: BLE001
        problems = [f"account check failed ({type(exc).__name__}: {exc})"]
    rest.new_http()                                      # the check's client belonged to a closed event loop
    if problems:
        store.event("error", f"{STRATEGY_ID}: live trading is OFF - {'; '.join(problems)}")
        return Runner(store, "live-blocked", blocked="; ".join(problems), notify_trades=notify)
    return Runner(store, "live", LiveBroker(rest), notify_trades=notify)


def serve() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    logging.getLogger("httpx").setLevel(logging.WARNING)
    runner = _runner()
    token = env_token()
    port = int(os.environ.get("PORT", "8080"))
    if not token:
        print("WARNING: CONTROL_TOKEN is empty - /v1/* answers 503 until it is set", flush=True)
    try:
        srv = make_server(runner, token, port)            # "::" = Railway private network (IPv6) + IPv4
    except OSError as exc:
        print(f"WARNING: cannot bind [::]:{port} ({exc}), falling back to 0.0.0.0", flush=True)
        srv = make_server(runner, token, port, host="0.0.0.0")

    def shutdown(*_):
        threading.Thread(target=srv.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    st = runner.store.data
    p = setup.P
    runner.store.event("info", f"{STRATEGY_ID} v{VERSION} started: mode {runner.mode}, {st['desired']}, "
                               f"{st['totals']['n']} trades so far ({st['totals']['pnl']:+.4f} USDT); "
                               f"{RISK['notional']:g} USDT/trade, approach >= {p['impulse_min']:.2%}/"
                               f"{p['impulse_secs']:.0f}s, take {p['take_frac']:.0%} of the move", True)
    runner.start()
    print(f"density-bounce: API on [::]:{port}, state {runner.store.path}", flush=True)
    srv.serve_forever()
    # open live trades keep their exchange-side stop and take; the next start reconciles them
    runner.store.save()
    print("density-bounce: stopped", flush=True)


def check_cmd() -> None:
    from .live import Rest, check
    problems = asyncio.run(check(Rest.from_env()))
    print("live check: OK" if not problems else "live check:\n- " + "\n- ".join(problems))


def main() -> None:
    a = sys.argv[1:]
    cmd = a[0] if a else "serve"
    if cmd == "serve":
        serve()
    elif cmd == "check":
        check_cmd()
    else:
        raise SystemExit(__doc__)


if __name__ == "__main__":
    main()
