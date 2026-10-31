#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

usage() {
  cat <<'EOF'
Usage: scripts/record-kora-mobile-demo.sh [--device DEVICE_ID]

Optional environment:
  ANDROID_SERIAL       Select an attached Android device/emulator
  MAESTRO_DEMO_TICKET_TITLE  Override the generated sample ticket title
EOF
}

device="${ANDROID_SERIAL:-}"
while (($#)); do
  case "$1" in
    --device)
      (($# >= 2)) || { usage >&2; exit 2; }
      device="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
done

command -v adb >/dev/null || { echo "adb is required on PATH" >&2; exit 1; }
command -v maestro >/dev/null || { echo "maestro is required on PATH" >&2; exit 1; }

if [[ -z "$device" ]]; then
  mapfile -t devices < <(adb devices | awk 'NR > 1 && $2 == "device" {print $1}')
  if ((${#devices[@]} != 1)); then
    echo "Expected one authorized Android device/emulator; found ${#devices[@]}." >&2
    echo "Connect one, or choose one with --device DEVICE_ID." >&2
    adb devices -l >&2
    exit 1
  fi
  device="${devices[0]}"
fi

adb -s "$device" get-state >/dev/null
run_id="$(date -u +%Y%m%dT%H%M%SZ)"
artifact_dir="$repo_root/artifacts/mobile-client-demo/$run_id"
ticket_title="${MAESTRO_DEMO_TICKET_TITLE:-Kora client demo $run_id}"
mkdir -p "$artifact_dir"
remote_video="/sdcard/Movies/kora-mobile-demo-$run_id.mp4"
record_pid=""

stop_recording() {
  if [[ -n "$record_pid" ]]; then
    adb -s "$device" shell "kill -INT $record_pid 2>/dev/null || true" >/dev/null 2>&1 || true
    sleep 2
    adb -s "$device" pull "$remote_video" "$artifact_dir/screen-recording.mp4" >/dev/null 2>&1 || true
    adb -s "$device" shell rm -f "$remote_video" >/dev/null 2>&1 || true
    record_pid=""
  fi
}
trap stop_recording EXIT INT TERM

echo "Recording on $device. Artifacts: $artifact_dir"
adb -s "$device" shell "mkdir -p /sdcard/Movies; screenrecord --bit-rate 8000000 --time-limit 180 '$remote_video' >/dev/null 2>&1 & echo \$!" > "$artifact_dir/screenrecord.pid"
record_pid="$(tr -d '\r\n' < "$artifact_dir/screenrecord.pid")"
sleep 2

set +e
maestro test --device "$device" \
  --test-output-dir "$artifact_dir/maestro" \
  -e "MAESTRO_DEMO_TICKET_TITLE=$ticket_title" \
  maestro/client_demo.yaml
maestro_status=$?
set -e

stop_recording
rm -f "$artifact_dir/screenrecord.pid"
if [[ -s "$artifact_dir/screen-recording.mp4" ]]; then
  echo "Screen recording saved to $artifact_dir/screen-recording.mp4"
else
  echo "Could not retrieve the screen recording from the device." >&2
  exit 1
fi
exit "$maestro_status"
