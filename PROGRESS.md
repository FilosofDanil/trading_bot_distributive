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
- [ ] Owner runs Desktop/Railway/enable_live.ps1 (Ed25519 keygen -> Binance -> hidden API key -> read-only check
      -> variables + redeploy); then Claude verifies both deploys + "LIVE" in the logs
