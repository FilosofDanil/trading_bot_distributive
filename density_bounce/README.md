# density_bounce — bounce off an order-book density

Runner `bounce`, strategy `bounce.density`. It watches 16 liquid (≥ 100M USDT a day), moving Binance USDⓈ-M perpetuals over the public
websockets (diff depth 100 ms + aggTrade), keeps a local order book for each, finds **densities** (large resting
orders) and trades the bounce off them. It speaks the runner API v1 ([`docs/runner_api.md`](../docs/runner_api.md)),
so `control` manages it from Telegram like `funding_carry`. **Paper mode by default: no order is sent.**

## The rules (the owner's, 2026-11-03)

| | rule (parameters in `bounce/setup.py` `P`) |
|---|---|
| density | resting order ≥ 8× the median level, ≥ 25k USDT (50k in v0.2.0), ≥ ½ minute of turnover (`bounce/book.py` `DENS`, the lab's detector); tracked from 3 % away and **≥ 60 s old** (`min_age`; 1.5 % and 20 s until v0.4.5), still ≥ 70 % of its peak, touched ≤ 2 times |
| **reality criterion** (v0.4.5) | the density must be worth **≥ 60 s of the aggressive flow that is hitting it now** (`hold_min`): its size / the one-sided tape toward it, the faster of the last 1 and 3 min. So it can hold price for a minute. Until v0.4.5 the size was compared with the average turnover only when the density was first seen (ZEC: a 50k wall against 2.6M a minute) |
| sharp approach | mid moved ≥ 0.80 % toward the density in the last 3 min, is still moving (≥ 0.15 % in the last 60 s, covering ≥ half of the distance it had 60 s ago); now within 0.35 % of it |
| previous move | from the extreme of the last 30 min (the high before a drop onto a bid wall, the low before a rally into an ask wall; 1m bars, the last hour is seeded when a symbol is added) to the density, ≥ 2.50 % |
| **entry quality** (v0.4.0, redefined in v0.4.5) | pumping coins are traded, the entry has to be good: against the 1h trend **before the move began** (the last hour close before it against the EMA20 of the hour closes; at the entry every bounce is counter-trend) only with **accel ≥ 1** — the speed of the last 3 min ≥ the average speed of the move itself (`counter_accel`), at placement and until the fill. A density that lost ≥ 20 % of its size in 10 s is being eaten: no entry (`eat_cancel`) |
| **market** (v0.4.5) | no entry when the other watched coins moved the same way: their median 30 min change ≥ 1 % toward the density (`mkt_max`). In a market-wide dump every wall is hit at once |
| entry | **armed first, placed after a confirmation** (v0.4.5): when price reaches the entry level (0.02 %, ≥ 1 tick, in front of the density) the bot waits 15 s (`confirm_secs`); if the density is still ≥ 70 % of its peak and not being eaten, a post-only limit goes in at that level (or one tick inside the opposite quote). `confirm_secs` 0 = placed at once, as before |
| **stop** | **max(0.15 %, 2 × ATR(14) of the 1m bars) behind the density** (≥ 2 ticks; `stop_buf`, `stop_atr`; 0.15 % only until v0.4.0). No ATR yet (fewer than 14 complete bars) = no trade; until v0.4.5 the plan fell back to 0.15 % |
| **size** | **a fixed USDT risk** (`RISK_USD`): qty = risk / stop distance, at most `NOTIONAL`. A wide stop is a smaller position |
| **take** | **the middle of the previous move** (50 % retracement from the density back toward the extreme) |
| filters | ≥ 100k USDT traded a minute (last 15 min of tape; `minute_min`) and ≥ 100M a day (the screener, 50M until v0.3.0), reward ≥ 1.0 × risk, risk 0.10–4 % of price (0.80 % until v0.4.0), tick ≤ 0.02 % of price (the screener too; 0.04 % until 2026-11-04: AKE at 2.5–3.3 bp lost 6 of 6), spread ≤ 0.06 %, the book within 0.3 % beyond the stop ≥ 3 × the position (`stop_depth_k`) |
| parts | **½ at +1R** (`tp1`; `tp1_r`, `tp1_part`), the rest at the take (`tp2`). With `PARAMS={"tp1_r":0,"tp1_frac":0.25,"tp1_part":0.25,"run_frac":1.0,"be_on_tp1":true}` the v0.2.0 parts: ¼ at 25 % of the move, 7/12 at the take, the last ⅙ runs to the start of the move (`run`); a part too small for the exchange is merged into `tp2` |
| break-even | **off**: the stop stays where it was when `tp1` fills (the owner's note "break-even too early"). `be_on_tp1` moves it to entry + 0.06 % (fees) when `tp1` fills; with a runner it moves to `tp1` when `tp2` fills; `be_at_r` > 0 moves it at that many R |
| exits | stop (`stop`; with break-even also `stop_be`, `stop_lock`), `take`, or market close after 60 min. No exit when the density disappears (`exit_on_gone`, off) |
| entry cancel | density gone / below 50 % of its peak / −20 % in 10 s (`density_eaten`) / worth < 30 s of the flow (`density_weak`, `hold_cancel`) / under 70 % at the confirmation (`unconfirmed`), an entry against the 1h trend whose approach stalled (`counter_stalled`), price 0.8 % away, or 3 min without a fill. After a cancel caused by the density itself that density is not tried again for 30 min (`wall_cool`); other cancels rest the symbol for 60 s (5 s until v0.4.5: HYPE and ARB were re-entered at the same wall a minute after a cancel) |
| plan journal (v0.4.5) | every plan is written to `<state>_plans.jsonl` (`GET …/plans?n=`): the traded ones with `act` = `filled` or `cancel:<why>`, and the ones a soft gate stopped (`skip:young`, `weak`, `counter_slow`, `market`, `thin_stop`). Each carries `shadow`: what the plain entry (a limit at once, never cancelled early, the same stop and takes) did on the tape — `hit` (nofill / stop / tp1+stop / take / time), net `r`, MFE, MAE. So a gate or a cancel is judged on every candidate, not only on the fills |
| slippage | each stop-out records `slip` (fill beyond the stop price) in the journal; 2 stops slipped > 0.2 % within 24 h → no trades on that symbol for 24 h (in memory: a restart forgets it) |

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

### v0.4.5 (2026-11-12): what a month of history says

v0.3.0 and v0.4.0 closed 30 trades without one full take. The critique and the numbers are in
[`docs/bounce_v045_2026-11-12.md`](../docs/bounce_v045_2026-11-12.md); the scripts in
`Engine/Strategy_tester/research/bounce_v045`. Binance keeps no order-book history, so the month was run in three
parts (R = result / risk, net of full fees; CI = 95 %, bootstrap by coin-day):

| what | sample | result |
|---|---|---|
| the trade without a wall: every "move ≥ 2.5 % / 30 min + approach ≥ 0.8 % / 3 min" on 1m klines, 69 coins, 10-12 → 11-12, the v0.4.5 stop / parts / take | 3835 | **−0.10R** [−0.13, −0.06], the same on both halves |
| real densities (the lab's week 10-24 → 10-31), v0.4.5 exits: density < 60 s of the flow | 501 | −0.17R [−0.29, −0.06]; the density went after 68 % of these fills |
| … density ≥ 60 s of the flow (the reality criterion) | 139 | −0.14R [−0.35, +0.07]; went after 41 % |
| … density and trade still there 15 s after the fill (the confirmed entry, approximated) | 149 | −0.00R [−0.19, +0.18] |
| … and ≥ 60 s of the flow, ≥ 60 s old | 50 | +0.03R [−0.32, +0.36] |
| the 73 live trades, their real result: density < 60 s / ≥ 60 s of the flow | 44 / 29 | −0.99R / −0.47R |
| the lab's own tick-level result (0.12 % stop, 2R take): density < 60 s / ≥ 60 s of the flow | 730 / 267 | −0.66R / −0.51R |

- **Age is the anti-spoof filter that works:** 91 % of the plans at a density younger than 60 s lost it before the
  fill (42 % at 60–300 s, 21 % older). 89 % of the densities that went after a fill went within 15 s, half within 1.3 s.
- **The gates remove losses, they do not add an edge.** Every sample moves toward zero, none is above it with any
  confidence, and the trade itself (no wall) costs 0.10R. All v0.4.5 gates together left 7 of the lab's 997 fills.
- Nothing separates on the klines: trend, accel and the market move change the result by a few hundredths of R.
  `counter_accel` and `mkt_max` stay as mild filters (against the trend with accel < 1: −0.12R against −0.07R; market
  ≥ 1 %: −0.13R against −0.10R), not as sources of profit.
- Not tested, because it needs the order book: the entry price of the confirmed entry, the 3 % tracking band.

Verdict: v0.4.5 is a cleaner experiment, not a working strategy. It runs in paper; the plan journal is what will
show whether any gate earns its keep.

Any parameter of `setup.P` or `book.DENS` can be changed without a code change:
`PARAMS={"take_frac":0.5,"usd_min":80000,"tp1_frac":0.25,"run_frac":1.0}` (unknown keys stop the start, so a typo
never passes silently).

## Money and safety

- `RISK_USD` lost at the stop (default 0.2 % of `NOTIONAL`: 0.30 USDT at 150, about the R of the v0.3.0 stop),
  `NOTIONAL` 10 USDT per trade at most, `LEVERAGE` 2 isolated, `MAX_OPEN` 2 trades at once, one per symbol, 60 s cooldown.
- No new entries for the UTC day after `DAY_LOSS` −1 USDT, `DAY_LOSS_R` −6 R (v0.4.5: the USDT limits on Railway
  were 15 stops a day) or `MAX_CONSEC_LOSSES` 6 losses in a row;
  `TOTAL_LOSS` −5 USDT halts the strategy for good (`state: error`). Pause / resume from Telegram is persisted.
- Live only: after `GUARD_N` 30 trades of the running version a mean below `GUARD_R` −0.3 R halts the strategy
  (the review's rule "back to paper", now in the code; `GUARD_N=0` switches it off).
- Live: the stop is an exchange-side `STOP_MARKET closePosition` (Algo API) placed the moment the entry fills, plus
  one reduce-only take per part; if the stop cannot be placed the position is closed at market. Client ids start with `dbb`;
  symbols with any position or order the bot did not place (manual, carry `fc*`, lab `skb*`) and BTC/ETH are never
  traded. Leftovers are cancelled / closed on the next start.
- The strategy reports `book: []` on purpose: control's LossGuard exempts `book` symbols of live strategies
  (hedged carry legs), and scalp positions must stay inside its loss measure. A LossGuard block pauses it like carry.
- Paper fills are optimistic about queue position. Since v0.4.5 placing and cancelling an entry take `SIM_LAG`
  0.3 s (an entry can fill while its cancel is on the way, as in live); the stop and the takes have no latency,
  so paper results are still an upper bound.

## Telegram (control)

`/bots`, `/bot bounce`, `/pause bounce`, `/resume bounce`, `/plan bounce` (what it sees now: books, nearest
densities, working entries), `/positions bounce`, `/report bounce` (trades, win rate, R, by exit reason / side /
day), `/run bounce` (the same snapshot as a message + an immediate re-screen), `/events bounce`, `/debug bounce`,
🛑 Close all (flatten). Each closed trade arrives as a message (`NOTIFY_TRADES=closes`; `all` adds fills, `none`).
Extra endpoints: `GET /v1/strategies/bounce.density/trades?n=50` — the journal with every trade's features (and,
since v0.4.5, `ver` and the plan id `pid`); `GET …/plans?n=200` — the plan journal.

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
| `RISK_USD` | 0.2 % of `NOTIONAL` | USDT a trade loses at its stop (before slippage and fees); the position is risk / stop distance, at most `NOTIONAL` |
| `DAY_LOSS_R`, `GUARD_N`, `GUARD_R` | 6, 30, −0.3 | day stop in R; live guard on the mean R of the running version (0 = off) |
| `SIM_LAG` | `0.3` | paper: seconds an entry needs to reach the exchange and a cancel to take effect |
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
