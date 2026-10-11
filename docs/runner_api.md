# Runner control API (v1)

Every trading-bot service ("runner") exposes this HTTP API on the Railway private network
(`http://<service>.railway.internal:8080`, IPv6, no public domain). `control` polls it and drives it
from Telegram. JSON in and out, UTF-8, times in ISO-8601 UTC (`2026-10-10T00:05:12Z`), days as `YYYY-MM-DD`.

## Auth

- `GET /healthz`: no auth, `200 {"ok": true}`. Used only by the Railway deploy healthcheck.
- Every `/v1/*` request carries `X-Control-Token: <CONTROL_TOKEN>`. If the token is wrong or missing, the
  runner answers `401 {"error": "unauthorized"}`. A runner started without `CONTROL_TOKEN` answers `503`
  to every `/v1/*` request.
- Errors: `{"error": "<text>"}` with 400 / 401 / 404 / 409 (busy) / 500.

## Endpoints

### `GET /v1/health`
```json
{"runner": "funding", "version": "0.1.0", "uptime_s": 8812, "mode": "paper", "now": "2026-10-10T11:32:05Z",
 "binance": {"ok": true, "latency_ms": 41, "error": null},
 "rss_mb": 118.4, "universe_days": 30, "state_bytes": 20480,
 "busy": false, "errors": [{"ts": "2026-10-10T00:05:40Z", "msg": "..."}]}
```
`errors` holds the last 5 errors (or is empty). `busy` is true while a daily run is in progress.

### `GET /v1/strategies` → `[Strategy]`
```json
{"id": "funding.carry", "name": "Funding carry (spot long + perp short)", "mode": "paper",
 "state": "running", "desired": "running",
 "open_positions": 2, "book": ["XUSDT", "YUSDT"],
 "capital0": 500.0, "equity": 501.23, "pnl_total": 1.23, "pnl_today": 0.05,
 "last_run_day": "2026-10-10", "last_run_at": "2026-10-10T00:05:40Z", "next_run_at": "2026-10-11T00:05:00Z",
 "last_error": null}
```
- `state` is one of `running`, `paused`, `busy`, `error`.
- `desired` is `running` or `paused`, as set from Telegram. It is persisted and survives restarts.
- `mode` is `paper` (simulated fills, no orders sent) or `live`.

### `POST /v1/strategies/{id}/pause` · `POST /v1/strategies/{id}/resume`
Body: `{"by": 891477091, "reason": "tg"}`. The response is `{"ok": true, "strategy": Strategy}`.
Pause means no new positions. Held positions are still managed: they can be closed, stopped or resized.

### `POST /v1/strategies/{id}/run`
Starts the daily cycle now, in the background. If today's day was already accepted, it computes a
preview plan only and does not change state. The response is `{"ok": true, "started": true, "preview": false}`,
or `409 {"error": "busy"}` if a run is already in progress.

### `POST /v1/strategies/{id}/flatten`
Kill switch, live mode only. Body `{"confirm": true, "by": 891477091}` (without `"confirm": true` → `400`).
Pauses the strategy, then closes every pair in the background; the result arrives as a `notify: true` event.
`409` if the runner is not live. Strategy `mode` is `paper`, `live`, or `live-blocked` (live requested, but the
key lacks permissions: no orders are sent). Control's LossGuard leaves the `book` symbols of `live` strategies
out of its loss measure and block enforcement.

### `GET /v1/strategies/{id}/plan`
The last plan, or `404` if there is none yet.
```json
{"day": "2026-10-10", "preview": false, "accepted": true, "paused": false, "equity": 501.2,
 "book": ["XUSDT"], "open": ["XUSDT"], "close": [], "resize": {"YUSDT": [40.1, 80.3]},
 "pair_stopped": [], "rehedge": [], "top": [["XUSDT", 0.00031], ["ZUSDT", 0.00012]],
 "min_f8": 0.0002, "skipped": {"QUSDT": "spot younger than 7 days"},
 "orders": [{"symbol": "XUSDT", "why": "funding 0.031 %/8h", "spot": ["BUY", 12.0], "perp": ["SELL", 12.0],
             "notional": 83.4, "margin_to_futures": 166.8, "ok": true}],
 "idle": 250.4, "computed_at": "2026-10-10T00:05:40Z"}
```
`top` lists the 5 highest 3-day mean funding rates per 8 h (as fractions: `0.0002` = 0.02 %).
It is computed over all candidates even while the strategy is paused. When `paused` is true, `book`/`open`
cover only the held pairs (no new ones) and `would_open` lists the pairs a resume would open; otherwise
`would_open` is `[]`. `candidates` is the size of the candidate universe. A finished preview emits a
`notify: true` event, so control forwards it to Telegram.

### `GET /v1/strategies/{id}/positions` → `[Position]`
```json
{"symbol": "XUSDT", "n": 83.4, "perp0": 1.234, "spot0": 1.231, "opened": "2026-10-08", "days": 2,
 "funding": 0.12, "basis": -0.03, "fees": -0.11, "pnl": -0.02}
```
`n` is the notional per leg in USDT. `funding`, `basis`, `fees` and `pnl` are cumulative USDT since the pair was opened.

### `GET /v1/strategies/{id}/report`
```json
{"start": "2026-10-10", "days": 12, "capital0": 500.0, "equity": 501.2, "return_pct": 0.24, "annual_pct": 7.5,
 "max_dd_pct": 0.1, "funding": 1.5, "basis": -0.2, "fees": -0.1,
 "pairs_opened": 3, "pairs_closed": 1, "pair_stops": 0, "active_days": 5,
 "curve": [["2026-10-10", 500.0], ["2026-10-11", 500.3]]}
```
`curve` holds at most the last 60 days. `annual_pct` is the return annualized by days (it is noisy for short records).

### `GET /v1/strategies/{id}/events?since=<seq>&n=<count>` → `[Event]`
```json
{"seq": 42, "ts": "2026-10-10T00:05:40Z", "level": "info", "msg": "paper: opened XUSDT pair 83.40 USDT", "notify": true}
```
- `level` is `info`, `warn` or `error`.
- `notify: true` marks events that control forwards to Telegram (trades, pair stops, errors, pause/resume).
- `since` returns only events with `seq > since`, oldest first. Without `since`, the last `n` events
  are returned (default 20, max 200), oldest first.
- `seq` keeps growing across restarts.

## Strategy ids
`<runner>.<strategy>`, for example `funding.carry`. Control lists the runners in
`RUNNERS=funding=http://funding-carry.railway.internal:8080[,name=url...]`.
