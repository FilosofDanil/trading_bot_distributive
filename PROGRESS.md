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
