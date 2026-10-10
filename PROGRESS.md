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
- [ ] Railway: create project from repo, Root Directory /control, volume /data, sealed variables (needs the account owner)
