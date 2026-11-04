"""Density-bounce runner: trades the bounce off a large resting order (density) on Binance USDⓈ-M perpetuals.

Rules (the owner's, 2026-11-03): enter when price makes a sharp approach to a density, stop just behind the
density, take profit at the middle of the move that brought price there. Control API v1: docs/runner_api.md.
"""
RUNNER = "bounce"
STRATEGY_ID = "bounce.density"
STRATEGY_NAME = "Density bounce (sharp approach, stop behind the wall, take at 50 % of the move)"
VERSION = "0.2.0"
