"""Density-bounce runner: trades the bounce off a large resting order (density) on Binance USDⓈ-M perpetuals.

Rules (the owner's, 2026-11-03): enter when price makes a sharp approach to a density, stop just behind the
density, take profit at the middle of the move that brought price there. Control API v1: docs/runner_api.md.
v0.4.0 (the owner's notes on 73 live trades, docs/bounce_review_2026-11-10.md): the stop is at least 2 x ATR(1m)
behind the density with a fixed USDT risk, half the position is taken at +1R, a bounce against the 1h trend needs a
sharp approach, and a density that is being eaten is not traded.
v0.4.5 (the critique of 2026-11-12, setup.py docstring): the density must be worth a minute of the flow that is
hitting it and be 60 s old, the entry is placed 15 s after price reached it if the density held, no entries in a
market-wide move, every plan and every cancel is journaled with what the plain entry would have done (shadow.py).
"""
RUNNER = "bounce"
STRATEGY_ID = "bounce.density"
STRATEGY_NAME = "Density bounce (sharp approach, stop behind the wall, take at 50 % of the move)"
VERSION = "0.4.5"
