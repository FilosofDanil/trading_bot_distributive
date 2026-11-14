# density-bounce runner. Build context: density_bounce/ (Railway Root Directory).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go test ./... && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bounce .

# distroless/static ships CA certificates (the Binance REST and websockets are TLS).
# Runs as root so it can write to the Railway volume mounted at /data.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/bounce /bounce
ENV STATE_FILE=/data/bounce_state.json PORT=8080 MODE=paper GOMEMLIMIT=64MiB
EXPOSE 8080
ENTRYPOINT ["/bounce"]
