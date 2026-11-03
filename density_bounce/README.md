# density_bounce — bounce off an order-book density

Runner `bounce`, strategy `bounce.density`. It watches 16 liquid, moving Binance USDⓈ-M perpetuals over the public
websockets (diff depth 100 ms + aggTrade), keeps a local order book for each, finds **densities** (large resting
orders) and trades the bounce off them. It speaks the runner API v1 ([`docs/runner_api.md`](../docs/runner_api.md)),
so `control` manages it from Telegram like `funding_carry`. **Paper mode by default: no order is sent.**

## The rules (the owner's, 2026-11-03)

| | rule (parameters in `bounce/setup.py` `P`) |
|---|---|
| density | resting order ≥ 8× the median level, ≥ 25k USDT, ≥ ½ minute of turnover (`bounce/book.py`, the lab's detector); alive ≥ 20 s, still ≥ 70 % of its peak, touched ≤ 2 times |
| sharp approach | mid moved ≥ 0.30 % toward the density in the last 60 s, covering ≥ half of the distance it had 60 s ago; now within 0.35 % of it |
| previous move | from the extreme of the last 5 min (the high before a drop onto a bid wall, the low before a rally into an ask wall) to the density, ≥ 0.50 % |
| entry | post-only limit 0.02 % (≥ 1 tick) in front of the density |
| **stop** | **0.15 % (≥ 2 ticks) behind the density** |
| **take** | **the middle of the previous move** (50 % retracement from the density back toward the extreme) |
| filters | reward ≥ 1.0 × risk, risk 0.10–0.80 % of price, tick ≤ 0.04 % of price, spread ≤ 0.06 % |
| exits | stop, take, or market close after 15 min. No exit when the density disappears (`exit_on_gone`, off) |
| entry cancel | density gone / below 50 % of its peak, price 0.8 % away, or 3 min without a fill |

Why there is no "density gone" exit: the lab's week of paper trading of a similar setup (Engine/Strategy_tester,
lab/book, 2026-10-24 → 10-31: 2R target, exit as soon as the wall disappears) closed 997 trades at −7.25 USDT,
win 10.9 %; 604 of them were "density gone" exits with 0 % wins. The stop behind the wall is the exit here.

Any parameter can be changed without a code change: `PARAMS={"take_frac":0.5,"stop_buf":0.002,"impulse_min":0.004}`
(unknown keys stop the start, so a typo never passes silently).

## Money and safety

- `NOTIONAL` 10 USDT per trade, `LEVERAGE` 2 isolated, `MAX_OPEN` 2 trades at once, one per symbol, 60 s cooldown.
- No new entries for the UTC day after `DAY_LOSS` −1 USDT or `MAX_CONSEC_LOSSES` 6 losses in a row;
  `TOTAL_LOSS` −5 USDT halts the strategy for good (`state: error`). Pause / resume from Telegram is persisted.
- Live: the stop is an exchange-side `STOP_MARKET closePosition` (Algo API) placed the moment the entry fills, plus a
  reduce-only take; if the stop cannot be placed the position is closed at market. Client ids start with `dbb`;
  symbols with any position or order the bot did not place (manual, carry `fc*`, lab `skb*`) and BTC/ETH are never
  traded. Leftovers are cancelled / closed on the next start.
- The strategy reports `book: []` on purpose: control's LossGuard exempts `book` symbols of live strategies
  (hedged carry legs), and scalp positions must stay inside its loss measure. A LossGuard block pauses it like carry.
- Paper fills are optimistic about queue position and ignore latency: paper results are an upper bound.

## Telegram (control)

`/bots`, `/bot bounce`, `/pause bounce`, `/resume bounce`, `/plan bounce` (what it sees now: books, nearest
densities, working entries), `/positions bounce`, `/report bounce` (trades, win rate, R, by exit reason / side /
day), `/run bounce` (the same snapshot as a message + an immediate re-screen), `/events bounce`, `/debug bounce`,
🛑 Close all (flatten). Each closed trade arrives as a message (`NOTIFY_TRADES=closes`; `all` adds fills, `none`).
Extra endpoint: `GET /v1/strategies/bounce.density/trades?n=50` — the journal with every trade's features.

## Variables

| Variable | Default | Notes |
|---|---|---|
| `CONTROL_TOKEN` | — | required, same value as on control (sealed) |
| `MODE` | `paper` | `live` sends real orders (see below) |
| `STATE_FILE` | `/data/bounce_state.json` | live: `/data/bounce_live_state.json` (the paper record stays) |
| `PAPER_CAPITAL` | `100` | starting equity of the paper record |
| `WATCH` | `16` | symbols watched at once (2 websockets each) |
| `NOTIONAL`, `LEVERAGE`, `MAX_OPEN`, `DAY_LOSS`, `TOTAL_LOSS`, `MAX_CONSEC_LOSSES`, `MAX_TRADES_DAY` | 10, 2, 2, 1, 5, 6, 60 | risk |
| `NOTIFY_TRADES` | `closes` | `all` / `closes` / `none` |
| `PARAMS` | — | JSON over `setup.P` |
| `BINANCE_API_KEY` + `BINANCE_API_SECRET` or `BINANCE_PRIVATE_KEY` | — | live only (sealed); Ed25519 PEM, `\n` may be literal |

Live mode starts only when `python -m bounce check` passes: futures trading enabled on the key, withdrawals off,
one-way position mode, available balance ≥ 2 × notional. Otherwise the runner reports `live-blocked` and opens
nothing. A Binance HMAC key without an IP whitelist is read-only and Railway Hobby has no static IP, so live needs
a self-generated **Ed25519** key (or a static IP).

## Run locally

```
cd density_bounce
pip install -r requirements.txt pytest
python -m pytest -q tests
CONTROL_TOKEN=x PORT=18080 STATE_FILE=./data/state.json python -m bounce serve
curl -H "X-Control-Token: x" localhost:18080/v1/strategies/bounce.density/plan
```

## Add to the Railway project (one time)

The same steps as funding-carry (README, round 2); in Git Bash set `MSYS_NO_PATHCONV=1` first.

```
railway add --service density-bounce
railway api 'mutation { serviceInstanceUpdate(serviceId:"<id>", environmentId:"<env>", input:{
  rootDirectory:"/density_bounce", dockerfilePath:"Dockerfile", watchPatterns:["/density_bounce/**"],
  numReplicas:1, overlapSeconds:0, drainingSeconds:30, restartPolicyType:ON_FAILURE,
  restartPolicyMaxRetries:10, healthcheckPath:"/healthz", healthcheckTimeout:120, sleepApplication:false}) }'
railway service scale --service density-bounce europe-west4-drams3a=1      # EU West (Binance answers 451 in the US)
railway api 'mutation { volumeCreate(input:{projectId:"<p>", serviceId:"<id>", environmentId:"<env>",
  mountPath:"/data", region:"europe-west4-drams3a"}) { id } }'
railway variable set CONTROL_TOKEN='${{control.CONTROL_TOKEN}}' --service density-bounce --skip-deploys
railway variable set 'RUNNERS=funding=http://funding-carry.railway.internal:8080,bounce=http://density-bounce.railway.internal:8080' \
  --service control --skip-deploys
railway service source connect --repo FilosofDanil/trading_bot_distributive --branch main --service density-bounce
railway service redeploy --service control --yes
```

Check: `railway logs --service density-bounce` shows `API on [::]:8080` and `watching 16: …`; in Telegram `/bots`
lists `bounce.density`. Budget: ~60–90 MB RAM and a few % of a vCPU (16 symbols × 2 websockets).
