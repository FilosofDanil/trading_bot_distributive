# trading_bot_distributive

Crypto capital and trading bot management via Telegram. Binance USDⓈ-M futures, hosted on Railway (Hobby, $5/month).

This is a clean version of the architecture. Working code is being moved here piece by piece from `Desktop/AI_Projects/Engine`
(`lossguard/`, `Trading_Bot/`, `journal/`). A detailed diagram is in
`Engine/docs/trading_infrastructure.html` (it should be moved to `docs/architecture.html`).

## Current state: round 1 — `control` on Railway

`control/` is the LossGuard Telegram bot ported from `Engine/lossguard`: it checks the account every 10 s,
alerts at a 10 % daily loss, and at 20 % cancels all orders, closes all positions and blocks trading for 24 h.
Commands so far: `/start`, `/status`, `/language`, `/stop`, `/cancel`, `/help`. State is still a JSON file
(`/data/state.json` on a Railway volume); Postgres comes in a later round.

**Secrets come only from the environment** (Railway service variables). No env file of any kind is in git:
`.gitignore` excludes `.env`, `.env.*` (including `.env.example`), `*.key`, `data/`, and CI fails if one is ever tracked.
A local `control/.env.example` exists only on the developer machine.

| Variable | Required | Notes |
|---|---|---|
| `TELEGRAM_BOT_TOKEN` | yes, sealed | from @BotFather |
| `ALLOWED_USER_IDS` | yes | comma-separated numeric Telegram ids (@userinfobot); mandatory when Binance keys are preset |
| `BINANCE_API_KEY`, `BINANCE_API_SECRET` | optional, sealed | separate key for Railway: Futures read + trade, no withdraw/transfer. Empty = `/start` asks in the chat |
| `ENCRYPTION_KEY` | recommended, sealed | 64 hex chars (`openssl rand -hex 32`); encrypts stored keys. Empty = generated into the volume |
| `TIMEZONE`, `DEFAULT_LANG` | optional | e.g. `Europe/Kyiv`, `en`/`ru`; defaults `UTC`, `en` |
| `ALERT_LOSS_PCT`, `BLOCK_LOSS_PCT`, `BLOCK_DURATION`, `PERIOD`, `ALERT_RESET_AFTER`, `CHECK_INTERVAL` | optional | defaults `10`, `20`, `24h`, `24h`, `1h`, `10s` |
| `STATE_FILE` | optional | image default `/data/state.json` |

Run locally: `cd control && go run .` (reads `control/.env` if present; `-check` validates config + token and exits).

Host on Railway (one time, in the dashboard):

1. New Project → Deploy from GitHub repo → this repo, region **EU West (Amsterdam)**.
2. Service settings: Root Directory `/control`, Dockerfile path `Dockerfile`, watch `/control/**`,
   1 replica, overlap 0 s, draining 30 s, restart on failure. Railway no longer accepts linking
   `railway.json` (config-as-code is deprecated), so these are set on the service directly;
   `control/railway.json` documents the intended values.
3. Add a **Volume** mounted at `/data` (the deploy refuses to start without it: `requiredMountPath`).
4. Variables: the table above, secrets sealed. Enable **Wait for CI**.
5. Deploy and check the log for `telegram: authorized as @<bot>`. Stop any local copy that uses the same
   bot token first: two pollers on one token fight over `getUpdates` (HTTP 409).

## What the system does

1. **Monitors the account**: balance, equity, daily PnL, drawdown from peak. Sends alerts to Telegram.
2. **Blocks trading** when a limit is reached: pauses the bots, cancels orders, closes positions,
   and blocks any new positions until the lock expires.
3. **Manages bots**: lists the available strategies, starts and pauses them, flattens positions.
4. **Builds reports**: one per strategy plus an overall report comparing strategies; manual trading is shown as a separate line.
5. **Backs up the database**: every day and on demand. The file is sent to Telegram and can be restored locally.

## Architecture

```
Telegram (you) ──Bot API, long polling──► control ──HTTP, private network──► runner ──► Binance
                                            │  └──────────► Binance (account monitoring, lock)
                                            ▼                    │
                                        Postgres ◄───────────────┘
                                            ▲
                                  backup (cron) ──► R2/B2 + Telegram
control ──ping/min──► healthchecks.io (external watchdog)
```

| Service | Language | Role | Ported from |
|---|---|---|---|
| `control` | Go | Telegram UI, risk guard (every 10 s), runner polling, reports, `/backup` | `Engine/lossguard` |
| `runner` | Python | strategy plugins, orders, control API on `[::]:8080` | `Engine/Trading_Bot/swingbot` |
| `backup` | shell / Go | cron `0 3 * * *` UTC: `pg_dump -Fc` → R2/B2 + Telegram | new |
| Postgres | — | single database (Railway template, 1 GB volume) | replaces `state.json` and `bot.db` |

Principles:

- **No public domains.** All connections go outbound: Telegram, Binance, R2, watchdog.
- **Desired state is stored in the database, commands are duplicated over HTTP.** The runner reads `strategies.desired_state` and `risk_state.locked_until`
  before every entry, so pauses and locks survive restarts and network failures.
- **The risk guard lives in a separate process** from the strategies it constrains.
- **Strategies run as plugins in a single runner**, not as a separate service each. This keeps us within budget.
- **Every position's stop is placed on the exchange.** A failure of any service never leaves a position unprotected.
- **Every order is tagged**: `clientOrderId = <strategy prefix>-<ts>`. The tag attributes a fill to a strategy,
  and anything without a tag is treated as manual trading (`manual`).

## Repository structure (planned)

```
control/                 Go module (Root Directory of the control service)
  migrations/            numbered SQL files, applied when control starts
  Dockerfile             with postgresql-client for /backup
  railway.json
runner/                  Python package (Root Directory of the runner service)
  strategies/            one plugin per strategy: bounce, breakout, …
  api.py                 control API
  Dockerfile | railway.json
backup/                  cron job (Root Directory of the backup service)
  Dockerfile | railway.json
docs/                    architecture.html, notes
.github/workflows/ci.yml go test + pytest; Railway waits for CI before deploying
```

> When a service has a Root Directory set, Railway builds **only that folder**. So code is not shared
> between services, and migrations live inside `control/`, not in the repo root.

## Telegram commands

| Command | Action |
|---|---|
| `/status` | account, daily PnL, drawdown, positions, lock |
| `/bots` | all strategies: status, positions, PnL today, time of last response; buttons ⏸ ▶️ 📊 |
| `/bot <id>` | strategy card; pause / start / flatten (with confirmation) / report |
| `/pause`, `/resume <id\|all>` | stop or restore new entries; open positions stay under exchange stops |
| `/report <id> [7d\|30d\|all]` | strategy report + PNG with the equity curve |
| `/compare [period]` | comparison of strategies and manual trading |
| `/limits`, `/lock` | limits (can be loosened only outside a lock and with a 24 h delay), manual lock |
| `/backup`, `/backups` | fresh database dump or the archive |

Access is checked by numeric `user_id` (`ALLOWED_USER_IDS`), not by username. Every command is recorded in `events`.

## Runner control API

```
GET  /v1/health                     → runner, version, uptime, leader, exchange/ws ok
GET  /v1/strategies                 → [{id, state, desired, mode, open_positions, pnl_today, last_error}]
POST /v1/strategies/{id}/pause      {"reason","by"}
POST /v1/strategies/{id}/resume     {"by"}
POST /v1/strategies/{id}/flatten    {"confirm": "<one-time token>"}
Header on every request: X-Control-Token
```

Strategy states: `running` · `paused` · `flattening` · `locked` · `stale` (no response > 60 s) · `offline` (> 5 min).

## Data (Postgres)

`strategies` · `runner_status` · `orders` · `fills` · `trades` · `equity_snapshots` · `cash_flows` · `risk_state` · `events` · `backups`.
API keys are not stored in the database.

## Backups and local restore

A backup is made every day at 03:00 UTC, before every migration, and on `/backup`. Format: `pg_dump -Fc --no-owner --no-acl`,
verified with `pg_restore --list`, with a sha256 file alongside.

```
docker run -d --name trading-pg -e POSTGRES_PASSWORD=pg -p 5432:5432 postgres:17
docker exec trading-pg createdb -U postgres trading
docker exec -i trading-pg pg_restore -U postgres -d trading --no-owner --clean --if-exists < trading_YYYY-MM-DD_0300.dump
```

The major version of `pg_dump` and `pg_restore` must be at least that of the server on Railway.

## Deploy: GitHub → Railway

1. The repository is **private**. In Railway: New Project → Deploy from GitHub repo, region **EU West (Amsterdam)**.
   Binance responds with 451 from US addresses.
2. The `control`, `runner`, and `backup` services are created from the same repo. For each, set the Root Directory, Watch Paths
   (`/control/**`, etc.) and the path to `railway.json`. Postgres comes from the template.
3. Variables (sealed): `TELEGRAM_BOT_TOKEN`, `ALLOWED_USER_IDS`, `BINANCE_API_KEY`, `BINANCE_API_SECRET`, `ENCRYPTION_KEY`,
   `CONTROL_TOKEN` (shared), `DATABASE_URL=${{Postgres.DATABASE_URL}}`,
   `RUNNERS=swingbot=http://runner.railway.internal:8080`, R2/B2 keys, `HEALTHCHECK_URL`.
4. Enable **Wait for CI**: a deploy starts only after tests are green in GitHub Actions.
5. Only verified code lands in `main`. To roll back, Redeploy a previous deployment in Railway.

Required on Railway:

- **Replicas = 1** and a leader-lock (`pg_try_advisory_lock`) in runner and control: during a deploy, two copies must not trade
  or poll Telegram at the same time. Set `RAILWAY_DEPLOYMENT_OVERLAP_SECONDS=0`.
- SIGTERM handling; `RAILWAY_DEPLOYMENT_DRAINING_SECONDS≈30`.
- The Python server listens on `::`, because Railway's private network runs over IPv6.
- Restart policy: On Failure. Railway checks the healthcheck only at deploy time; liveness is tracked by control and the external watchdog.
- **Do not enable** the hard usage limit (it will stop the services) or App Sleeping.
- Hobby has no static IP, so a **separate Binance key for Railway** is needed: Futures trade, no withdraw or transfer.

Estimated budget ≈ $3.8–5.1/month: control ~40 MB, runner ~150 MB, Postgres 80–150 MB, backup ~1 min/day.
Check against railway.com/pricing and the Usage tab after the first week.

## Secrets — never in git

`.env`, `config.txt`, `*.key`, `state.json`, `*.db`, `*.log`, `data/`, candles and datasets, `*.exe`. The root `.gitignore`
is added before the first commit. If a key has ever landed in the repository, it must be reissued.

## Migration plan

- [ ] 0 · `.gitignore` ✅, CI ✅, private GitHub repo, Railway project in EU West, Postgres, separate Binance key
- [ ] 1 · `control` ← `lossguard` ✅ (round 1: `ALLOWED_USER_IDS` ✅, keys from env ✅, Dockerfile + `railway.json` ✅); still to do: state → Postgres + migrations, `/healthz`, watchdog, equity snapshots, `cash_flows`
- [ ] 2 · `runner` ← `swingbot`: journal → Postgres, `desired_state` instead of the STOP file, `risk_lock` check, control API, leader-lock, `clientOrderId` tags; one week in paper mode
- [ ] 3 · runner polling, `/bots`, pause / resume / flatten, fills import (`journal`), `/report`, `/compare`, 23:59 digest
- [ ] 4 · `backup` service, `/backup`, `/backups`, first test restore

Open questions:

- A single account or a separate sub-account for the bots?
- Does the lock also close manual positions (as it does now) or only bot positions?
- Drawdown-from-peak threshold and per-strategy limits.
