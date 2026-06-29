# Stage 1: Build React SPA
FROM oven/bun:1-alpine AS ui
WORKDIR /app/ui
COPY ui/package.json ui/bun.lock* ./
RUN bun install --frozen-lockfile
COPY ui/ ./
RUN bun run build

# Stage 2: Build Go binary (static, no CGO)
FROM golang:1.25-bookworm AS go
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
COPY --from=ui /app/ui/dist ./workspace/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X github.com/asenawritescode/kora/cli.Version=${VERSION}" -o kora .

# Stage 3: Runtime (Alpine for small static binary)
FROM alpine:3.21
RUN apk add --no-cache ca-certificates curl tzdata
COPY --from=go /app/kora /usr/local/bin/kora
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD curl -sf http://localhost:8000/api/ping || exit 1
ENTRYPOINT ["kora", "serve", "--port", "8000"]
