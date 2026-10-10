"""Funding-carry runner: market-neutral spot long + perp short on coins that pay positive funding.

Ported from Engine/Strategy_tester/lab/carry_live.py (the recommended "safe carry": buffer_calm + pair stop
+ delisting exit). PAPER mode only: fills are simulated at the daily open, no order is ever sent.
"""

VERSION = "0.1.0"
RUNNER = "funding"
STRATEGY_ID = "funding.carry"
STRATEGY_NAME = "Funding carry (spot long + perp short)"
