# Stage 1: Build Go binary (static, no CGO). The Engine is API-only; the UI is deployed separately.
FROM golang:1.25-bookworm AS go
WORKDIR /app
COPY go.mod go.sum ./
COPY sdk/go.mod sdk/go.sum ./sdk/
RUN go mod download
COPY . ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X github.com/asenawritescode/kora/internal/cli.Version=${VERSION}" -o kora ./cmd/kora

# Stage 2: Runtime (Alpine for small static binary)
FROM alpine:3.21
RUN apk add --no-cache ca-certificates curl tzdata
COPY --from=go /app/kora /usr/local/bin/kora
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD curl -sf http://localhost:8000/readyz || exit 1
ENTRYPOINT ["kora", "serve", "--port", "8000"]
