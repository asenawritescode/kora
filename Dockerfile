# Stage 1: Build React SPA
FROM oven/bun:1-alpine AS ui
WORKDIR /app/ui
COPY ui/package.json ui/bun.lock* ./
RUN bun install --frozen-lockfile
COPY ui/ ./
RUN bun run build

# Stage 2: Build Go binary
FROM golang:1.25-bookworm AS go
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
COPY --from=ui /app/ui/dist ./workspace/dist
ARG CGO_ENABLED=1
RUN CGO_ENABLED=${CGO_ENABLED} go build -ldflags="-s -w" -o kora .

# Stage 3: Runtime (Debian slim for glibc — required by go-libsql's Rust CGO bindings)
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
COPY --from=go /app/kora /usr/local/bin/kora
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD curl -sf http://localhost:8000/api/ping || exit 1
ENTRYPOINT ["kora", "serve", "--port", "8000"]
