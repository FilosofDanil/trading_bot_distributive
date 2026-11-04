# density_bounce — bounce off an order-book density

Runner `bounce`, strategy `bounce.density`. It watches 16 liquid (≥ 100M USDT a day), moving Binance USDⓈ-M perpetuals over the public
websockets (diff depth 100 ms + aggTrade), keeps a local order book for each, finds **densities** (large resting
orders) and trades the bounce off them. It speaks the runner API v1 ([`docs/runner_api.md`](../docs/runner_api.md)),
so `control` manages it from Telegram like `funding_carry`. **Paper mode by default: no order is sent.**

## The rules (the owner's, 2026-11-03)

| | rule (parameters in `bounce/setup.py` `P`) |
|---|---|
| density | resting order ≥ 8× the median level, ≥ 25k USDT (50k in v0.2.0), ≥ ½ minute of turnover (`bounce/book.py` `DENS`, the lab's detector); alive ≥ 20 s, still ≥ 70 % of its peak, touched ≤ 2 times |
| sharp approach | mid moved ≥ 0.80 % toward the density in the last 3 min, is still moving (≥ 0.15 % in the last 60 s, covering ≥ half of the distance it had 60 s ago); now within 0.35 % of it |
| previous move | from the extreme of the last 30 min (the high before a drop onto a bid wall, the low before a rally into an ask wall; 1m bars, the last hour is seeded when a symbol is added) to the density, ≥ 2.50 % |
| entry | post-only limit 0.02 % (≥ 1 tick) in front of the density |
| **stop** | **0.15 % (≥ 2 ticks) behind the density** |
| **take** | **the middle of the previous move** (50 % retracement from the density back toward the extreme) |
| filters | ≥ 100k USDT traded a minute (last 15 min of tape; `minute_min`) and ≥ 100M a day (the screener, 50M until v0.3.0), reward ≥ 1.0 × risk, risk 0.10–0.80 % of price, tick ≤ 0.02 % of price (the screener too; 0.04 % until 2026-11-04: AKE at 2.5–3.3 bp lost 6 of 6), spread ≤ 0.06 % |
| parts | the whole position goes at the take: parts are off (`tp1_frac` 0, `run_frac` 0). With `PARAMS={"tp1_frac":0.25,"run_frac":1.0}`: ¼ at 25 % of the move (`tp1`), 7/12 at the take (`tp2`), the last ⅙ runs to the start of the move (`run`); a part too small for the exchange is merged into `tp2` |
| break-even | off with one take. With parts: when `tp1` fills the stop moves to entry + 0.06 % (fees); when `tp2` fills it moves to `tp1` for the runner. `be_at_r` > 0 also moves it at that many R |
| exits | stop (`stop`; with parts also `stop_be`, `stop_lock`), `take`, or market close after 60 min. No exit when the density disappears (`exit_on_gone`, off) |
| entry cancel | density gone / below 50 % of its peak, price 0.8 % away, or 3 min without a fill |

Why there is no "density gone" exit: the lab's week of paper trading of a similar setup (Engine/Strategy_tester,
lab/book, 2026-10-24 → 10-31: 2R target, exit as soon as the wall disappears) closed 997 trades at −7.25 USDT,
win 10.9 %; 604 of them were "density gone" exits with 0 % wins. The stop behind the wall is the exit here.

### Why these thresholds (v0.3.0, 2026-11-04)

The owner's complaints: illiquid coins, windows and takes too short, too many noisy trades. Checked by replaying
the lab's 997 paper fills (2026-10-24 → 10-31, 54 coins) and the 45 live trades on 1m klines, with the bot's stop,
fees and 2 bp stop slippage; one position per symbol; a stop and a take in one bar count as the stop.

With 40 % of the commission returned (`FEE_REBATE`: maker 2 → 1.2 bp, taker 5 → 3 bp):

| rule on the paper fills (`MAX_OPEN` 2 for v0.3.0, 300 USDT a trade) | trades in 7 days | mean net per trade | 95 % CI (by coin-day) | total | total, full fees |
|---|---|---|---|---|---|
| v0.2.0 signal and exits | 131 | −4.0 bp | −8.7 … +0.9 | −15.61 USDT | −25.39 USDT |
| v0.3.0 rules, density ≥ 50k, takes in parts | 31 | +18.2 bp | −11.5 … +56.9 | +16.88 USDT | +14.47 USDT |
| v0.3.0 rules, density ≥ 25k, takes in parts | 42 | +25.4 bp | +3.4 … +61.0 | +32.03 USDT | +28.79 USDT |
| **v0.3.0 as configured** (density ≥ 25k, one take at 50 %) | 43 | +33.3 bp | +10.3 … +70.2 | +42.90 USDT | +39.65 USDT |

The bullets below were measured with full fees.

- **Move and approach decide.** The net result grows steadily with both the move (30 min) and the approach (3 min)
  on both halves of the period. Measuring either in units of the coin's own volatility was worse.
- **The window and the take work only together with that selection:** on all fills, windows from 5 to 120 min all
  stay negative. On the selected trades a take at 25 % of the move earns +5 bp, at 50 % to 100 % +25 to +50 bp.
- **Liquidity:** the floors of 100M a day and 100k a minute cost nothing here, but volume does not predict the result
  by itself, and 200M a day made the second half negative.
- **The first part at +1 R and break-even at +0.7 R hurt** (+1 R is noise on a 0.17 % risk): on the same trades they
  give +12.8 / −5.1 bp on the two halves. Even ¼ at 25 %, 7/12 at 50 %, ⅙ at 100 % of the move cost 6 to 10 bp a
  trade against one take at 50 %, so the owner chose one take (2026-11-04); parts stay available through `PARAMS`.
- **A stop further from the density was worse** on the second half at every width tried (0.25 %, 0.40 %, 0.60 %).

Limits: few trades, about one trade in four wins, the profit comes from a few big winners (without the top three
the 43 trades as configured earn +7.8 bp), and this variant and its thresholds were chosen with both halves in view,
so the second half is not a clean test. None of the 45 live trades passes the new filter, so the live record says nothing about it.
Paper fills ignore the queue. Expect long losing streaks (9 here, 18 under nearby thresholds):
`MAX_CONSEC_LOSSES` must be set with that in mind.

Any parameter of `setup.P` or `book.DENS` can be changed without a code change:
`PARAMS={"take_frac":0.5,"usd_min":80000,"tp1_frac":0.25,"run_frac":1.0}` (unknown keys stop the start, so a typo
never passes silently).

## Money and safety

- `NOTIONAL` 10 USDT per trade, `LEVERAGE` 2 isolated, `MAX_OPEN` 2 trades at once, one per symbol, 60 s cooldown.
- No new entries for the UTC day after `DAY_LOSS` −1 USDT or `MAX_CONSEC_LOSSES` 6 losses in a row;
  `TOTAL_LOSS` −5 USDT halts the strategy for good (`state: error`). Pause / resume from Telegram is persisted.
- Live: the stop is an exchange-side `STOP_MARKET closePosition` (Algo API) placed the moment the entry fills, plus
  one reduce-only take per part; if the stop cannot be placed the position is closed at market. Client ids start with `dbb`;
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
| `FEE_REBATE` | `0` | share of the commission the account gets back (`0.4` = 40 %). Journal, reports and loss limits count fees net of it; each trade keeps the amount in `rebate`. The bot does not check that the money arrived |
| `NOTIFY_TRADES` | `closes` | `all` / `closes` / `none` |
| `PARAMS` | — | JSON over `setup.P` and `book.DENS` |
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
