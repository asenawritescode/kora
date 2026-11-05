#!/bin/sh
set -eu

cd "$(dirname "$0")"

# Read simple KEY=VALUE lines without `source`, since DB_DSN query strings
# contain ampersands and must not be interpreted as shell syntax.
if [ -f .env.local ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in ''|'#'*) continue ;; esac
    key=${line%%=*}
    value=${line#*=}
    case "$key" in
      DB_DSN|KORA_DB_TYPE|KORA_DB_HOST|KORA_DB_PORT|KORA_DB_USER|KORA_DB_PASSWORD|KORA_RELOAD_TOKEN|KORA_ENGINE_CONFIG_TOKEN|KORA_ENGINE_PROVISIONING_TOKEN|KORA_ENGINE_CLOUD_IDENTITY_URL|KORA_ENGINE_CLOUD_IDENTITY_TOKEN|KORA_ENGINE_MANAGEMENT_TOKEN|KORA_ENGINE_MANAGEMENT_CALLER_ID|KORA_ENGINE_DEPLOYMENT_ID|KORA_ENGINE_DEPLOYMENT_URLS|KORA_ENGINE_MANAGEMENT_DATABASE_ALLOCATION_REF|KORA_ENGINE_MANAGEMENT_ARTIFACT_URL|KORA_ENGINE_MANAGEMENT_ARTIFACT_TOKEN|KORA_ENGINE_LOCAL_DEPLOYMENT_ID|KORA_CLOUD_AI_GATEWAY_URL|KORA_CLOUD_AI_GATEWAY_TOKEN|KORA_CLOUD_AI_GATEWAY_MODEL|KORA_SMTP_HOST|KORA_SMTP_PORT|KORA_SMTP_USERNAME|KORA_SMTP_PASSWORD|KORA_SMTP_FROM|KORA_SMTP_TLS_MODE|KORA_PUBLIC_APP_BASE_URL)
        export "$key=$value"
        ;;
    esac
  done < .env.local
fi

: "${DB_DSN:?Set DB_DSN in .env.local before restarting the local Engine}"
if [ -n "${KORA_CLOUD_AI_GATEWAY_URL:-}" ] && [ -z "${KORA_CLOUD_AI_GATEWAY_TOKEN:-}" ]; then
  echo "KORA_CLOUD_AI_GATEWAY_URL is set, but its service token is missing." >&2
  exit 1
fi
if [ -n "${KORA_ENGINE_MANAGEMENT_CALLER_ID:-}" ]; then
  : "${KORA_ENGINE_MANAGEMENT_TOKEN:?Engine management caller requires KORA_ENGINE_MANAGEMENT_TOKEN}"
  : "${KORA_ENGINE_DEPLOYMENT_ID:?Engine management caller requires KORA_ENGINE_DEPLOYMENT_ID}"
  : "${KORA_ENGINE_MANAGEMENT_DATABASE_ALLOCATION_REF:?Engine provisioning requires KORA_ENGINE_MANAGEMENT_DATABASE_ALLOCATION_REF}"
fi

engine_bin="/tmp/kora-engine-restart-$$"
go build -o "$engine_bin" .

old_pids=$(pgrep -f 'kora.*serve --port 8000$' || true)
if [ -n "$old_pids" ]; then
  for pid in $old_pids; do kill -TERM "$pid"; done
  attempt=0
  while [ "$attempt" -lt 15 ]; do
    running=false
    for pid in $old_pids; do
      if kill -0 "$pid" 2>/dev/null; then running=true; fi
    done
    [ "$running" = false ] && break
    attempt=$((attempt + 1))
    sleep 1
  done
  if [ "$running" = true ]; then
    echo "The existing Engine did not stop cleanly; not starting a second instance." >&2
    exit 1
  fi
fi

setsid nohup env -i \
  PATH="$PATH" \
  DB_DSN="$DB_DSN" \
  KORA_DB_TYPE="${KORA_DB_TYPE:-mysql}" \
  KORA_DB_HOST="${KORA_DB_HOST:-127.0.0.1}" \
  KORA_DB_PORT="${KORA_DB_PORT:-3306}" \
  KORA_DB_USER="${KORA_DB_USER:-root}" \
  KORA_DB_PASSWORD="${KORA_DB_PASSWORD:-}" \
  KORA_RELOAD_TOKEN="${KORA_RELOAD_TOKEN:-}" \
  KORA_ENGINE_CONFIG_TOKEN="${KORA_ENGINE_CONFIG_TOKEN:-}" \
  KORA_ENGINE_PROVISIONING_TOKEN="${KORA_ENGINE_PROVISIONING_TOKEN:-}" \
  KORA_ENGINE_CLOUD_IDENTITY_URL="${KORA_ENGINE_CLOUD_IDENTITY_URL:-}" \
  KORA_ENGINE_CLOUD_IDENTITY_TOKEN="${KORA_ENGINE_CLOUD_IDENTITY_TOKEN:-${KORA_ENGINE_PROVISIONING_TOKEN:-}}" \
  KORA_ENGINE_MANAGEMENT_TOKEN="${KORA_ENGINE_MANAGEMENT_TOKEN:-}" \
  KORA_ENGINE_MANAGEMENT_CALLER_ID="${KORA_ENGINE_MANAGEMENT_CALLER_ID:-}" \
  KORA_ENGINE_DEPLOYMENT_ID="${KORA_ENGINE_DEPLOYMENT_ID:-}" \
  KORA_ENGINE_DEPLOYMENT_URLS="${KORA_ENGINE_DEPLOYMENT_URLS:-}" \
  KORA_ENGINE_MANAGEMENT_DATABASE_ALLOCATION_REF="${KORA_ENGINE_MANAGEMENT_DATABASE_ALLOCATION_REF:-}" \
  KORA_ENGINE_MANAGEMENT_ARTIFACT_URL="${KORA_ENGINE_MANAGEMENT_ARTIFACT_URL:-}" \
  KORA_ENGINE_MANAGEMENT_ARTIFACT_TOKEN="${KORA_ENGINE_MANAGEMENT_ARTIFACT_TOKEN:-}" \
  KORA_ENGINE_LOCAL_DEPLOYMENT_ID="${KORA_ENGINE_LOCAL_DEPLOYMENT_ID:-}" \
  KORA_CLOUD_AI_GATEWAY_URL="${KORA_CLOUD_AI_GATEWAY_URL:-}" \
  KORA_CLOUD_AI_GATEWAY_TOKEN="${KORA_CLOUD_AI_GATEWAY_TOKEN:-}" \
  KORA_CLOUD_AI_GATEWAY_MODEL="${KORA_CLOUD_AI_GATEWAY_MODEL:-cloud-managed}" \
  KORA_SMTP_HOST="${KORA_SMTP_HOST:-}" \
  KORA_SMTP_PORT="${KORA_SMTP_PORT:-0}" \
  KORA_SMTP_USERNAME="${KORA_SMTP_USERNAME:-}" \
  KORA_SMTP_PASSWORD="${KORA_SMTP_PASSWORD:-}" \
  KORA_SMTP_FROM="${KORA_SMTP_FROM:-}" \
  KORA_SMTP_TLS_MODE="${KORA_SMTP_TLS_MODE:-auto}" \
  KORA_PUBLIC_APP_BASE_URL="${KORA_PUBLIC_APP_BASE_URL:-}" \
  KORA_CSRF_SECURE=false \
  KORA_LOG_FORMAT=text \
  "$engine_bin" serve --port 8000 > /tmp/kora_server.log 2>&1 < /dev/null &
engine_pid=$!
attempt=0
while [ "$attempt" -lt 60 ]; do
  if curl --fail --silent --show-error http://127.0.0.1:8000/api/ping >/dev/null 2>&1; then
    echo "Kora Engine is healthy (pid $engine_pid). Logs: /tmp/kora_server.log"
    exit 0
  fi
  if ! kill -0 "$engine_pid" 2>/dev/null; then
    echo "Kora Engine exited before becoming healthy. Recent log output:" >&2
    tail -40 /tmp/kora_server.log >&2 || true
    exit 1
  fi
  attempt=$((attempt + 1))
  sleep 1
done

echo "Kora Engine did not become healthy within 60 seconds; leaving it running for diagnosis. Logs: /tmp/kora_server.log" >&2
exit 1
