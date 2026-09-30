# syntax=docker/dockerfile:1.7
FROM node:24.21.0-bookworm-slim@sha256:0e0ff40c39bc087845bfb27465a0df4ea419520094bc35842ff83dd8cbe6f9b6 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS build
WORKDIR /src
ENV CGO_ENABLED=1
RUN apt-get update && apt-get install -y --no-install-recommends gcc g++ libc6-dev && rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN --mount=type=cache,target=/root/.cache/go-build go build -trimpath -ldflags='-s -w' -o /out/codex-pool .

FROM scratch AS binary
COPY --from=build /out/codex-pool /codex-pool

FROM build AS verify
RUN useradd -u 1001 -m verify && chown -R verify:verify /src
USER verify
ENV GOCACHE=/home/verify/.cache/go-build
RUN --mount=type=cache,target=/home/verify/.cache/go-build,uid=1001,gid=1001 go vet ./... && go test -race -count=1 ./...

FROM debian:12.15-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 1000 codex && useradd -u 1000 -g codex --home-dir /app codex
WORKDIR /app
COPY --from=build /out/codex-pool /app/codex-pool
RUN mkdir -p /app/data /app/pool /app/tmp && chown -R codex:codex /app
USER codex
ENV DUCKDB_PATH=/app/data/usage.duckdb
EXPOSE 8989
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD curl -fsS http://127.0.0.1:8989/healthz || exit 1
ENTRYPOINT ["/app/codex-pool", "-env-file="]
