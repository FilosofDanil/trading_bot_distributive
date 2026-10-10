# control — Telegram bot + risk guard. Build context: control/ (Railway Root Directory).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go test ./... && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/control .

# distroless/static ships CA certificates; IANA zones are embedded via time/tzdata.
# Runs as root so it can write to the Railway volume mounted at /data.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/control /control
ENV STATE_FILE=/data/state.json
ENTRYPOINT ["/control"]
