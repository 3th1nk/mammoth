#!/bin/sh
# Agent initramfs pilot e2e runner (docs/12-agent-initramfs.md):
# builds the binary, starts a private mammoth against the dev PG, and runs
# scripts/agent-dev/e2e.py for both firmwares. See README in this directory.
set -e
cd "$(dirname "$0")/../.."

REPO=$PWD
WORK=${AGENT_E2E_WORK:-/tmp/agent-e2e}
ISO=${AGENT_E2E_ISO:-$HOME/mammoth-qxe/alpine-extended-3.22.2-x86_64.iso}
API=${AGENT_E2E_API:-http://127.0.0.1:18080}
DSN=${AGENT_E2E_DSN:-postgres://mammoth:mammoth@localhost:55432/agent_e2e?sslmode=disable}
FIRMWARES=${AGENT_E2E_FIRMWARES:-"bios uefi"}

mkdir -p "$WORK/media" "$WORK/logs"
pkill -f "agent-e2e/mammoth serve" 2>/dev/null || true
pkill -f "agent-dev/e2e.py" 2>/dev/null || true
pkill -f qemu-system 2>/dev/null || true
sleep 1
echo "· building mammoth"
go build -o "$WORK/mammoth" ./cmd/mammoth

cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null
}
trap cleanup EXIT INT TERM

start_server() {
  MAMMOTH_DATABASE_URL="$DSN" \
  MAMMOTH_HTTP_ADDR="127.0.0.1:18080" \
  MAMMOTH_API_TOKEN=devtoken \
  MAMMOTH_MASTER_KEY=Th+pC1GHDGZvSD30cuffK4re9MM1d6wOqmSdMcW0/Ow= \
  MAMMOTH_MEDIA_DIR="$WORK/media" \
  MAMMOTH_EXTERNAL_URL=http://10.0.2.2:18080 \
  MAMMOTH_MEDIA_BASE_URI=nfs://127.0.0.1/mammoth-media \
  "$WORK/mammoth" serve --mode=all >"$WORK/logs/server.log" 2>&1 &
  SERVER_PID=$!
  for i in $(seq 1 60); do
    curl -sf -o /dev/null "$API/healthz" && return 0
    sleep 0.5
  done
  echo "mammoth did not become healthy — server.log:"
  tail -20 "$WORK/logs/server.log"
  return 1
}

rc=0
start_server
for fw in $FIRMWARES; do
  echo "═══ firmware: $fw ═══"
  python3 scripts/agent-dev/e2e.py \
    --api "$API" --token devtoken \
    --iso "$ISO" --media-dir "$WORK/media" \
    --workdir "$WORK/$fw" --firmware "$fw" || rc=1
done
exit $rc
