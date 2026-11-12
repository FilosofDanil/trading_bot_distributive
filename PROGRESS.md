# Round 1 — host the Telegram bot (control ← Engine/lossguard)

- [x] Root .gitignore (all env files incl. .env.example, data/, keys, state, db, logs, binaries)
- [x] Copy lossguard → control/ (no .env, data/, exe), module `control`
- [x] Config: secrets from env only (drop BINANCE_KEYS_FILE), ALLOWED_USER_IDS (numeric), Railway-friendly defaults
- [x] control/.env.example — local only, verified ignored by git
- [x] Dockerfile + railway.json for control
- [x] CI: .github/workflows/ci.yml (gofmt + go vet + go test, no-secrets check)
- [x] go vet / go test / docker build pass; `-check` authorizes with Telegram
- [x] README: round-1 deploy steps; migration plan ticks
- [x] Commit + push; verify no env file in git
- [x] Railway: project `trading_bot_distributive` (EU West), service `control` ← GitHub main, Root Directory /control,
      volume /data, variables TELEGRAM_BOT_TOKEN / ALLOWED_USERNAMES / ENCRYPTION_KEY → deploy SUCCESS,
      log `telegram: authorized as @testing_claude_bot`
- [x] ALLOWED_USER_IDS=891477091 set → deployment 6c48af81 SUCCESS
- [ ] Owner: seal the variables in the dashboard, enable Wait for CI, live /start test

# Round 2 — funding-carry runner + bot management/debug commands (goal 2026-10-10)

Source: Engine/Strategy_tester/lab/carry_live.py (+ carry.py, universe.py, data.py); safe carry "buffer_calm"
with pair stop −2 % / 7 d cooldown, delisting exit; honest CAGR +3.1 %, DD 0.9 % (reports/critique, smallmoney).
PAPER mode only: the lab rule is live orders only after explicit approval + IP-locked key (no static IP on Hobby).

- [x] docs/runner_api.md — runner control API contract (v1)
- [x] funding_carry/: market.py, universe.py, strategy.py (decide/orders verbatim), paper.py (accept + daily PnL),
      state.py (atomic JSON on /data), runner.py (daily 00:05 UTC loop), api.py (stdlib HTTP on ::), __main__.py
- [x] tests: 11 pytest (strategy rules, paper PnL, API auth/pause/events); decide() == lab decide() on 1000
      random markets (0 differences; books 638, pair stops 237, re-hedges 188, resizes 319)
- [x] live paper cycle 2026-10-10 against Binance: universe 31 d x 525 perps, 156 candidates, top funding
      0.012 %/8h < 0.02 % -> book empty (as the lab's own plans of 10-08/10-09); 704 s on the first start
- [x] Dockerfile (tests run at build), requirements.txt (lab versions), railway.json (documented), README
- [x] control: internal/runners client + poller (alerts), commands /bots /bot /pause /resume /run /plan
      /positions /report /events /debug /ping, inline buttons, i18n EN/RU, config RUNNERS / CONTROL_TOKEN
- [x] gofmt / go vet / go test; docker build both; e2e Go client ↔ real Python runner (internal/runners/e2e_test.go)
- [x] CI: python job for funding_carry
- [x] commit ec03d1f + push to main (funding_carry/ in its own folder)
- [x] Railway CLI: service funding-carry (root /funding_carry, EU West, volume /data, CONTROL_TOKEN, healthcheck
      /healthz) ← GitHub main → deployment 36695a9d SUCCESS, log "API on [::]:8080", Binance reachable (no 451);
      control RUNNERS + CONTROL_TOKEN → redeploy a79ea9fd SUCCESS, "runners: 1 configured, poll every 30s";
      network log: control → funding-carry:8080 TCP OK every 30 s
- [ ] Owner: in Telegram /bots, /plan funding, /debug (first universe build ~12 min after each fresh volume)

# Bugfix 2026-10-10 — inline buttons did not work → reply (text) keyboards

- [x] control/internal/bot/keyboards.go: main menu (Status, Bots, Help, Language), /bots list ("🤖 <id>"),
      per-bot controls ("<label> · <id>": pause, resume, run, plan, positions, report), language choice;
      per-chat keyboard view; presses parsed from message text; old inline presses re-send the menu
- [x] removed InlineKeyboard / callback routing (runners.go, bot.go); getUpdates names allowed_updates explicitly
- [x] tests: keyboards_test.go (round-trip of every button, views); go vet + go test pass
- [x] Railway redeploy of control: deployment 1692ecd7 (commit 878f84d) SUCCESS
- [ ] Owner: in Telegram tap 🤖 Bots → a bot → buttons

# Bugfix 2026-10-10 — empty plan after pause + "Run", bot looked stuck

Cause (runner log): day 10-10 accepted with 156 candidates, top funding 0.012 %/8h < min 0.02 % → book empty;
then pause; then two "Run" presses → previews with `cands = []` while paused → 0 candidates, no top,
overwrote the day's plan; preview event had notify=false → no Telegram message ("hangs"). No resume was sent.

- [x] funding_carry/runner.py: candidates always computed; paused plan = held pairs only, plus the full decision
      for `top` and `would_open`; preview event notify=true with top funding + min threshold
- [x] tests/test_runner.py: paused preview keeps top + would_open, running preview decides once (13 pytest pass)
- [x] control: Plan.WouldOpen/Candidates; plan notes (paused: would open …; empty book below min funding);
      "Run" while paused explains it; preview reply says the result arrives as a message
- [x] control: button texts compared without U+FE0F (▶️ Resume / ⬅️ Menu from clients that strip it)
- [x] docs/runner_api.md: would_open, candidates, preview notify
- [x] commit + push → Railway redeploys control and funding-carry; verify logs (df607c2 SUCCESS both)

# Round 3 — LIVE carry with liquidation protection (owner decision 2026-10-11)

Owner decisions: Binance key WITHOUT IP restriction, stay on Railway Hobby; MAIN account (LossGuard must be
hedge-aware); 500 USDT live capital, max 3 coins; fully automatic opens. Live stays OFF (MODE=paper) until
the owner sets the key on funding-carry and says to switch.

Liquidation protection (short perp leg), in order of price rise:
  L0 margin buffer: isolated 1x, total margin 2N per leg N (liquidation ~ +195 %)
  L1 watchdog every 30 s: liq distance < 60 % → add isolated margin (futures wallet, then spot transfer)
  L2 watchdog: perp >= +90 % since ref or liq distance < 25 % → close the pair (both legs), 7-day cooldown
  L3 exchange-side STOP_MARKET (close short) at +120 % from ref — works while the bot is down
  L4 leg check: a leg missing / qty mismatch > 5 % → close the other leg / fix, alert
  L5 kill switch: 🛑 Close all (with confirmation) → flatten + pause

- [x] research: Binance API details → scratchpad/binance_live_api.md. FINDING: an HMAC key without an IP
      whitelist is READ-ONLY (Binance support article 2025-03-20) → use a self-generated Ed25519 key
      (unconfirmed without IP) or a static IP. Conditional orders: Algo Service only (-4120 on /fapi/v1/order).
- [x] funding_carry/carry/exchange.py — signed spot/fapi/sapi client, HMAC + Ed25519, permission check
- [x] funding_carry/carry/live.py — executor (open/close/resize/rehedge with real fills, undo on failure) + watchdog L1–L4
- [x] runner: MODE=live wiring, separate state file, MAX_COINS, startup reconcile, flatten API, live-blocked
- [x] tests: test_live.py (fake exchange, 14) + test_exchange.py (mock HTTP, 8): 35 pytest pass
- [x] control/guard: exempt live carry symbols (upnl, enforcement), adjust loss for transfers + carry income
      (guard/carry.go, binance.IncomeSince, carry_test.go: pumped short / carry close / block spares the pair)
- [x] control: LossGuard block → pause carry opens; unblock → resume (only if control paused it) (bot/carry.go)
- [x] control: 🛑 Close all button with confirmation (2 min); live / live-blocked labels
- [x] docs + README; gofmt/vet/test, pytest, docker build both; live gate vs real Binance → live-blocked
- [ ] commit + push, deploy in paper mode; owner sets key → switch MODE=live

# Round 4 — density-bounce runner (goal 2026-11-03)

Owner's rules: trade off order-book densities on a sharp approach; stop behind the density; take at the middle
of the previous move. Source: Engine/Strategy_tester/lab/book (book.py density detector verbatim; feed, SimBroker,
LiveBroker adapted). Lab paper week 10-24..10-31 (2R target, exit on "density gone"): 997 trades, -7.25 USDT,
win 10.9 %; 604 "density_gone" exits = 0 % win -> the owner's rules drop that exit and size the take by the move.

- [x] density_bounce/bounce: book.py (lab), setup.py (entry / stop / take = 50 % of the move), feed.py, sim.py,
      trader.py (lifecycle + risk), live.py (signed, HMAC/Ed25519, exchange stop), state.py, api.py, __main__.py
- [x] tests: setup rules, sim fills, trader lifecycle on a synthetic book, API, live broker with a fake exchange (25 pass)
- [x] Dockerfile (tests at build: 25 pass in the image, 223 MB), requirements.txt, railway.json, README; CI job
- [x] control: generic `text` for plan / positions / report (carry unchanged); gofmt / vet / test pass
- [x] docs/runner_api.md + README round 4; local paper run against live Binance (16 books synced, first trade
      HOTUSDT short -> take +2.03R); container: control e2e test (Go client <-> runner) PASS, RSS 88 MB
- [x] commit e3413b3 + push; Railway CLI (owner's goal 2026-11-03): folder re-linked (repo moved to Desktop/Railway),
      service density-bounce 4c0b679e (root /density_bounce, EU West, volume /data, healthcheck /healthz),
      CONTROL_TOKEN=${{control.CONTROL_TOKEN}} (reference), control RUNNERS += bounce=...:8080, source -> GitHub main
- [x] deployments density-bounce dfd5fe32 + control 5f353e59 (e3413b3) SUCCESS 2026-11-03 14:31 UTC; bounce log
      "API on [::]:8080", watching 16; control "runners: 2 configured", TCP to 2 runners :8080 every 30 s
- [x] Owner decision 2026-11-03: BOTH bots live, 500 USDT each; bounce 500 USDT per trade, x2, max 2, day -15,
      total -75 (account has >= 1000 USDT). Fix 03a625e: live check needs max_open x notional / leverage margin.
- [x] Owner: key from control's store instead of a new one -> f917c31 control keyserver (KEY_SERVER_PORT=8090,
      X-Control-Token) + runners KEY_FROM_CONTROL; control log "keyserver: serving", 1 active account
- [x] runners MODE=live (carry 500 / 3 coins; bounce 500 per trade x2, max 2, day -15, total -75) -> SUCCESS, both
      live-blocked, no order: carry key lacks Universal Transfer; bounce futures available 365.17 < 500
- [x] 12225f6: live used the paper STATE_FILE (Dockerfile ENV) -> LIVE_STATE_FILE /data/*_live_state.json
- [x] Owner 2026-11-03: no Universal Transfer, volumes 300 -> a70bd42 carry no-transfer mode (each wallet pays
      its own leg), empty live records follow the capital setting; carry CARRY_CAPITAL=300, bounce CAPITAL=300
      NOTIONAL=300 DAY_LOSS=9 TOTAL_LOSS=45; deployed SUCCESS both, logs "mode live" (carry 42, bounce 30 pytest)
- [ ] Owner: keep ~100 USDT on spot (carry spot legs) and ~500 on futures (carry ~200 + bounce margin 300)
- [x] 2026-11-04 journal of the first 45 live trades (artifact) + analysis: no entry feature separates wins;
      17 of 31 stops had been >= +1 R; AKE (tick 2.5-3.3 bp) 0/6, -5.9 USDT
- [x] Owner 2026-11-04: bounce v0.2.0 - density >= 50k USDT, tick <= 0.02 % (setup + screener), takes in parts
      1/2 at +1 R, 1/3 at the take, 1/6 runner to the move start; break-even at +0.7 R or tp1, runner stop at tp1
      after tp2; book.DENS overridable via PARAMS; 37 pytest pass
- [x] deploy v0.2.0: deployment 77117247 (24a11be) SUCCESS 2026-11-04 11:49 UTC, log "v0.2.0 started: mode live,
      running, 45 trades (-3.3431)", watching 16 (no coarse-tick coins), no leftovers to close
- [x] Owner 2026-11-04: MAX_CONSEC_LOSSES=12 and unblock today's entries: the block now keeps the limit it was set
      under (`consec_block_limit`), raising the limit lifts it; 38 pytest pass
- [x] first live trade with parts (ZEC short 2026-11-04 16:09 UTC): split and tp1 work; the stop move failed with
      -4130 (one closePosition stop per side), the rest stopped at the initial stop, labelled stop_be
- [x] fix: move_stop cancels the old stop first, restores it if the new one fails, closes if neither can be placed;
      stop_be / stop_lock only after a move that reached the exchange; fake exchange models -4130; 42 pytest pass
- [x] deploy the fix: deployment d2630d95 (103758d) SUCCESS 2026-11-04 16:53 UTC, mode live
- [ ] check the log "stop -> ... (stop_be)", no -4130, and a real stop_be / stop_lock exit near its stop (no trade has
      moved its stop since the fix: UNI and ZEC longs stopped out without reaching a part)
- [x] Owner 2026-11-04: rework the signal (illiquid coins, short windows and takes, noisy trades). Study: 997 paper
      fills + 45 live trades replayed on 1m klines (density_bounce/README.md, "Why these thresholds")
- [x] bounce v0.3.0 in the working tree, not committed: move >= 2.5 % / 30 min, approach >= 0.8 % / 3 min, 100M a day and
      100k a minute floors, 1m bars seeded from klines, parts 1/4 at 25 % / 7/12 at 50 % / 1/6 at 100 % of the move,
      break-even only on tp1, 60 min hold; 50 pytest pass; 5 min paper smoke on the public feed: 16 synced, no errors
- [x] deploy v0.3.0: e2bada5, deployment 759b9659 SUCCESS 2026-11-04 17:54 UTC, log "v0.3.0 started: mode live, paused,
      52 trades (-7.0373)", watching 16, no leftovers, no errors; the stop move stays unproven, off with one take
- [ ] Owner: /resume bounce; then check the first trades; judge after about 40 trades
- [x] Owner 2026-11-04: the most profitable variant of the replay: density >= 25k, one take at 50 % of the move; parts,
      runner and break-even off by default (PARAMS tp1_frac, run_frac turn them on); 52 pytest pass
- [x] Owner 2026-11-04: MAX_CONSEC_LOSSES=20 set on Railway without a redeploy (applies with the v0.3.0 deploy)
- [x] Owner 2026-11-04: 40 % of the commission comes back: FEE_REBATE in the journal, reports and limits (new trades
      only), FEE_REBATE=0.4 set on Railway without a redeploy; replay table recalculated; 53 pytest pass
- [x] first v0.3.0 trades 2026-11-05 04:03-05:03 UTC: 6 of 6 stops, -5.31 USDT (4 shorts into a JCT pump, 2 AIN); one take
      for the whole size and fees at 60 % confirmed (0.126 of 0.210 USDT); a filled take is still unproven
- [x] Owner 2026-11-06: NOTIONAL=150, DAY_LOSS=4.5; owner redeployed: 2abdfb40 SUCCESS 07:33 UTC, log "v0.3.0 started: mode
      live, running, 63 trades (-19.2834); 150 USDT/trade", no leftovers, no errors
- [ ] owner compares the fees with the rebate actually received
- [ ] Owner to decide: after a stop no new trade on that coin and side for 60 min (replay: profit unchanged, 7 fewer
      trades, losing streak 7 instead of 8)

## bounce v0.4.0 (2026-11-10, plan docs/bounce_review_2026-11-10.md; owner: pumps stay, the entry must be good)
- [x] book.py: 1h closes (seeded + tape), ATR(1m), trend EMA20 1h, price 30 min ago, 3 min turnover, density size history
- [x] setup.py: stop max(stop_buf, 2 x ATR), risk_max 4 % (1.5 % kept 11 of 21 v0.3 trades at -0.56R); counter-trend
      needs accel >= 1 at placement and until the fill (counter_stalled); tp1 at +1R for 1/2, no BE; skip / cancel
      when the density lost >= 20 % in 10 s; stop_depth(); new features
- [x] trader.py: size from RISK_USD (default 0.2 % of NOTIONAL), capped by NOTIONAL; depth check before the entry;
      BE on tp1 only with be_on_tp1; slip in the journal, 24 h ban after 2 stops slipped > 0.2 % (in memory)
- [x] feed.py: 1h klines with the 1m seed, ch24 on the book; runner / __main__ lines; VERSION 0.4.0
- [x] tests for every rule: 64 pytest pass (53 before)
- [x] README (rules table, variables), review doc
- [x] paper smoke on the public feed (2 + 5 min: 16 synced, h1 48 each, ATR / trend / ch24 / accel computed, no errors;
      0 plans in 5 min; JCT ATR 3.8-4.4 %, INIT 2.3-2.8 %: 2 x ATR > risk_max, those stay out while that wild)
- [x] Owner 2026-11-10: "commit and deploy in paper". No open bot position (last live trade 13:49 UTC, the UNI entry
      expired); MODE=paper set with --skip-deploys; commit 9796061 pushed; deployment 990d9eea SUCCESS 14:32 UTC, log
      "v0.4.0 started: mode paper, running, 9 trades so far (-0.0861 USDT); 0.3 USDT a stop", watching 16, no errors
- [ ] paper: >= 50 trades, diary review (counter / accel / slip / depth_x in the features); then decide on live
      (v0.4.0 paper 11-10 14:32 -> 11-12 06:45 UTC: 26 plans, 9 fills, 0 full takes, 1 tp1, -2.57 USDT, -0.96R a trade)

## bounce v0.4.5 (2026-11-12, owner: "fix the errors of the critique, add a reality criterion for the density
## (it must be able to hold price for a minute), roll out 0.4.5, run it on the last month, say if it works")
Study scripts and data: Engine/Strategy_tester/research/bounce_v045 (fetch.py, study_*.py, out/*.json).
- [x] data: 1m klines 2026-10-12 -> 11-12, 69 coins + BTC (49 with >= 5 liquid days + the lab's and the bot's coins)
- [x] study A (klines, no wall): 3835 filled events, -0.10R [-0.13, -0.06], the same on both halves; trend, accel,
      market move shift it by hundredths of R (out/study_a.txt)
- [x] study B (real walls): 4618 lab plans, 997 fills re-traded with the v0.4.5 exits: density < 60 s of the flow
      -0.17R (n 501), >= 60 s -0.14R (n 139); density + trade alive 15 s after the fill -0.00R (n 149), with
      hold >= 60 and age >= 60 +0.03R (n 50, CI -0.32..+0.36); all gates 7 fills; age < 60 s: 91 % of the plans lost
      the density before the fill; 89 % of the densities gone after a fill went within 15 s (out/study_b.txt)
- [x] study C: the lab's own tick-level R by the same features (out/study_c.txt)
- [x] setup.py / book.py: reality criterion (hold_min 60, hold_cancel 30), ATR missing = no trade, accel against the
      move's own speed, trend before the move, min_age 60 s with a 3 % tracking band, market filter (mkt_max 1 %),
      confirmed entry (confirm_secs 15), soft gates -> shadow plans (scan)
- [x] trader.py / state.py / shadow.py: plan journal (<state>_plans.jsonl, GET .../plans: act, skip, shadow outcome),
      a density that failed is closed for 30 min, ver + pid in every record, DAY_LOSS_R, live guard (GUARD_N / GUARD_R)
- [x] sim.py: SIM_LAG 0.3 s on placing and cancelling an entry
- [x] tests (sonnet-worker): 137 pytest pass (65 before); README; docs/bounce_v045_2026-11-12.md; VERSION 0.4.5
- [x] local paper smoke on the public feed: 7 min as configured (16 synced, lag 0.7 s, no errors, 0 plans) and 10 min
      with loosened PARAMS (4 armed, 5 cancels with reasons, 15 plan records: skip:weak / young / counter_slow,
      cancel:entry_timeout / counter_stalled; no errors; no entry reached the confirmation, that path has unit tests)
- [x] commit b274b57 pushed; deployment badeffa3 SUCCESS 2026-11-12 19:25 UTC, log "v0.4.5 started: mode paper,
      running, 25 trades so far (-2.4565 USDT)", watching 16, no errors; the docs commit d1b03de redeployed it:
      39390f32 SUCCESS 19:28 UTC, the same start line
- [x] v0.4.0 paper final (11-10 14:32 -> 11-12 19:25 UTC): 36 plans, 16 trades, 2 full takes, -0.50R a trade,
      -2.37 USDT; by the logged features v0.4.5 would have skipped both takes (a 20 s old wall, a wall of 0.18 of
      the minute turnover) and three of that day's stops
- [x] verdict to the owner: no positive expectation on any sample; paper only
- [ ] paper v0.4.5: >= 200 plan records, then judge the gates by `skip` / `act` / `shadow.r` (GET .../plans);
      stop rule: mean R < -0.3 after 30 trades, or the gated plans' shadow result no better than the skipped ones'
- [ ] not done: the live broker still polls REST (0.4 s) instead of the user-data stream
