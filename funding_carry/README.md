# funding_carry — funding-carry runner (PAPER)

A market-neutral **funding carry** strategy: a spot long and a perp short of equal quantity on coins whose
perpetual pays positive funding, so the short collects funding while the price risk cancels out. This is a
port of `Engine/Strategy_tester/lab/carry_live.py`, the "safe carry" the lab recommends:

- **Honest backtest, 3 years, delisted coins included:** CAGR **+3.1 %**, Sharpe 4.1, max DD 0.9 %
  (`reports/critique`, `reports/smallmoney`). For the 8 unseen quarters: +0.26 %/month, the worst quarter −0.02 %.
- **Real stop.** A pair is closed when its basis result since entry reaches **−2 % of the leg**, and the coin
  is then blocked for 7 days. Pairs are also closed a week before the perp delists.
- **Rules.** The score is the 3-day mean funding per 8 h. A pair opens at ≥ 0.02 % and is kept while ≥ 0.01 %,
  at most 8 coins. A new pair needs |basis| ≤ 2 % and no daily range above 35 % in the last 14 days.
  Each leg has notional N, and the perp margin is 2N (isolated, 1x). A held pair is resized only outside a
  60 % band. A pair is re-hedged after the perp rises +100 %.
- **Candidates.** These are the coins tagged in the last 30 days by the lab's two universes: coins in play
  (gainers / losers / decorrelated / volume_in) and the liquid top-40, provided they have a USDT spot pair
  that has traded for at least 7 days.

`carry/strategy.py` keeps `decide()` and `orders()` line for line with the lab. Both implementations were run
on 1000 random markets and agreed on every one, covering opens, the keep band, pair stops, re-hedges, resizes
and cooldowns.

## Paper mode only

The runner simulates fills at the daily open, the same way the backtest does, and **never sends an order**.
`MODE` accepts only `paper`. Live execution needs three things: the owner's explicit approval, an
IP-locked Binance key (Railway Hobby has no static outbound IP), and a few weeks of paper record with a
non-empty book. The paper PnL follows `lab/carry.run_carry`: the funding settled on the short, plus the
basis change, minus fees (spot taker 0.10 % + perp taker 0.05 % + 2 × 0.02 % slippage per pair entry or exit).

## Daily cycle (00:05 UTC)

1. Fill the missing days of the 30-day universe window. At the first start this builds all 31 days from
   the hourly bars of every perp, about 1000 public REST calls (~5 min). After that it builds one day per day.
2. `decide()`. While **paused**, no new pairs are opened, and held pairs are still kept, resized, stopped or closed.
3. Paper fills, marks and fees, then the state is saved to the volume and events are written. Control forwards
   to Telegram every event that has `notify: true`.

## API

See [`../docs/runner_api.md`](../docs/runner_api.md). The base URL is `http://funding-carry.railway.internal:8080`,
and every `/v1/*` request needs `X-Control-Token`. `GET /healthz` is open.

## Run locally

```
cd funding_carry
pip install -r requirements.txt pytest
python -m pytest -q tests
set STATE_FILE=data\carry_state.json
python -m carry once            # one cycle now (uses the public Binance API)
python -m carry once --preview  # the plan only, the state is not changed
set CONTROL_TOKEN=dev & python -m carry serve
```

## Railway

The service is `funding-carry`, with Root Directory `/funding_carry`, the Dockerfile builder, 1 replica and
a volume at `/data`. The image runs the tests at build time.

| Variable | Notes |
|---|---|
| `CONTROL_TOKEN` | shared with control (sealed) |
| `CARRY_CAPITAL` | paper capital in USDT, default 500. Used only when the state file is first created |
| `RUN_AFTER_MIN` | minutes after 00:00 UTC, default 5 |
| `STATE_FILE`, `PORT`, `MODE` | image defaults: `/data/carry_state.json`, `8080`, `paper` |

The region must not be in the US, because Binance answers 451 there. The runner raises `RestrictedLocation`,
and control shows it as the strategy error.
