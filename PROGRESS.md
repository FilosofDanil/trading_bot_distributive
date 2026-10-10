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
