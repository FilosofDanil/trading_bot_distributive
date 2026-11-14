# trading_bot_distributive: rules for Claude

## Tech stack: Go (standard for this repo, owner's decision 2026-11-14)

- Every service in this repo is written in **Go**: `control`, `funding_carry`, `density_bounce` and any new one.
  Go 1.24, stdlib first. A third-party module only when the stdlib has no equivalent
  (`gorilla/websocket` for Binance streams, the Telegram bot API).
- **Python (or any other runtime) only when Go is physically impossible**: no Go library or protocol
  implementation exists for something the service must do, and writing one is not feasible. Write the reason
  into this file before adding such a service.
- Why: Railway bills resident RAM by the GB-hour. In `../PRICING.md` (2026-11-14), 74 % of the bill for the two
  Python runners was RAM: 431 MB and 271 MB. A Go service in this account uses about 20 MB (`control`).

## Memory rules

RAM costs about $10 per GB a month on Railway. A volume costs $0.15 per GB a month, about 67 times less.
100 MB of RAM costs as much as 0.05 vCPU of constant load.

- Keep only what a decision reads. Rolling windows are fixed buckets or ring buffers, not raw event tapes.
  No unbounded maps keyed by price or time. Prune on a timer.
- Compact types for bulk data: int32 price ticks, and float32 quantities or prices where 7 significant digits
  are enough (a test proves the decision is unchanged). Timestamps are int64 ms or uint32 s. Money in the state and
  in order maths stays float64.
- Cold data (journals, history) goes to the `/data` volume (JSON / JSONL), not into memory.
- Build each Dockerfile in two stages: `golang` then `distroless/static`. Set `GOMEMLIMIT` for every service.

## Conventions

- Each service is its own Go module in its own folder (= its Railway Root Directory). Services do not share code.
- `gofmt`, `go vet` and `go test ./...` must pass. CI and the Docker build both run the tests.
- The runner HTTP contract is in `docs/runner_api.md`. State files on `/data` stay backward compatible: a new
  version must read the file the previous one wrote. A live runner may hold real positions.
- Commit, push and deploy only when the owner says so. A push to `main` deploys to Railway.
