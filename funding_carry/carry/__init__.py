"""Funding-carry runner: market-neutral spot long + perp short on coins that pay positive funding.

Ported from Engine/Strategy_tester/lab/carry_live.py (the recommended "safe carry": buffer_calm + pair stop
+ delisting exit). MODE=paper (default) simulates fills at the daily open; MODE=live trades on the owner's
account with the liquidation guard of carry/live.py.
"""

VERSION = "0.1.0"
RUNNER = "funding"
STRATEGY_ID = "funding.carry"
STRATEGY_NAME = "Funding carry (spot long + perp short)"
