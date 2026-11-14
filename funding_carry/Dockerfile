# funding-carry runner. Build context: funding_carry/ (Railway Root Directory).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
# the image is built only if the tests pass
RUN CGO_ENABLED=0 go test ./... && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/funding-carry .

# distroless/static ships CA certificates. Runs as root so it can write to the Railway volume mounted at /data.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/funding-carry /funding-carry
ENV STATE_FILE=/data/carry_state.json PORT=8080 MODE=paper GOMEMLIMIT=48MiB
EXPOSE 8080
ENTRYPOINT ["/funding-carry"]
CMD ["serve"]
