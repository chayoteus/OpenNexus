#!/bin/bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"

MESSENGER_URL="${MESSENGER_URL:-http://127.0.0.1:8080}"
MESSENGER_URL2="${MESSENGER_URL2:-http://127.0.0.1:8081}"
START_LOCAL_SERVERS="${START_LOCAL_SERVERS:-1}"

port_in_use() {
  local port="$1"
  ss -ltn "( sport = :$port )" | grep -q ":$port"
}

next_free_port() {
  local port="$1"
  while port_in_use "$port"; do
    port=$((port + 1))
  done
  echo "$port"
}

if [[ "$START_LOCAL_SERVERS" == "1" ]]; then
  PORT1="${MESSENGER_URL##*:}"
  PORT2="${MESSENGER_URL2##*:}"

  if port_in_use "$PORT1" || port_in_use "$PORT2"; then
    PORT1=$(next_free_port 18080)
    PORT2=$(next_free_port $((PORT1 + 1)))
    MESSENGER_URL="http://127.0.0.1:${PORT1}"
    MESSENGER_URL2="http://127.0.0.1:${PORT2}"
    echo "Default messenger ports occupied; switched to $MESSENGER_URL and $MESSENGER_URL2"
  fi
fi

export MESSENGER_URL MESSENGER_URL2

echo "T1 messenger: $MESSENGER_URL"
echo "T2 messenger: $MESSENGER_URL2"

PASS=0
FAIL=0
RECV1_PID=""
RECV2_PID=""
MS1_PID=""
MS2_PID=""

cleanup() {
  [[ -n "$RECV1_PID" ]] && kill "$RECV1_PID" 2>/dev/null || true
  [[ -n "$RECV2_PID" ]] && kill "$RECV2_PID" 2>/dev/null || true
  [[ -n "$MS1_PID" ]] && kill "$MS1_PID" 2>/dev/null || true
  [[ -n "$MS2_PID" ]] && kill "$MS2_PID" 2>/dev/null || true
  rm -f /tmp/t1.pub /tmp/t1.priv /tmp/t2.pub /tmp/t2.priv

  if [[ "${KEEP_LOGS_ON_FAIL:-1}" == "1" && "$FAIL" -gt 0 ]]; then
    echo "Preserving /tmp/t*.log for debugging because failures were detected."
  else
    rm -f /tmp/t*.log
  fi
}
trap cleanup EXIT

check() {
  local desc="$1" file="$2" pattern="$3"
  if grep -q "$pattern" "$file" 2>/dev/null; then
    echo "  PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $desc (expected '$pattern' in $file)"
    FAIL=$((FAIL + 1))
  fi
}

wait_health() {
  local base_url="$1"
  for _ in $(seq 1 30); do
    if curl -fsS "$base_url/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}

if [[ "$START_LOCAL_SERVERS" == "1" ]]; then
  if command -v go >/dev/null 2>&1; then
    PORT1="${MESSENGER_URL##*:}"
    PORT2="${MESSENGER_URL2##*:}"

    (cd "$ROOT_DIR" && PORT="$PORT1" REDIS_ADDR="" go run ./cmd/messenger > /tmp/messenger1.log 2>&1) &
    MS1_PID=$!
    (cd "$ROOT_DIR" && PORT="$PORT2" REDIS_ADDR="" go run ./cmd/messenger > /tmp/messenger2.log 2>&1) &
    MS2_PID=$!

    wait_health "$MESSENGER_URL" || { echo "Failed to start messenger1"; cat /tmp/messenger1.log || true; exit 1; }
    wait_health "$MESSENGER_URL2" || { echo "Failed to start messenger2"; cat /tmp/messenger2.log || true; exit 1; }
    echo "Local messenger servers started."
  else
    echo "go not found; skip local server startup (set START_LOCAL_SERVERS=0 explicitly to silence this)."
  fi
fi

wait_health "$MESSENGER_URL" || { echo "Messenger not healthy: $MESSENGER_URL"; exit 1; }
wait_health "$MESSENGER_URL2" || { echo "Messenger not healthy: $MESSENGER_URL2"; exit 1; }

supports_v1_messages() {
  local base_url="$1"
  local code
  code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$base_url/v1/messages" -H 'Content-Type: application/json' -H 'X-Agent-ID: probe' -d '{}')
  [[ "$code" != "404" ]]
}

requires_public_key_header() {
  local base_url="$1"
  local body
  body=$(curl -sS -X POST "$base_url/v1/messages" -H 'Content-Type: application/json' -H 'X-Agent-ID: probe' -d '{}' || true)
  [[ "$body" == *"X-Public-Key header is required"* ]]
}

if ! supports_v1_messages "$MESSENGER_URL"; then
  echo "Messenger missing /v1/messages: $MESSENGER_URL"
  exit 1
fi

if requires_public_key_header "$MESSENGER_URL"; then
  echo "Messenger API mismatch at $MESSENGER_URL: server requires X-Public-Key, but current Python client uses X-Agent-ID."
  echo "Run against a compatible messenger build or update client/server header contract first."
  exit 1
fi

if ! supports_v1_messages "$MESSENGER_URL2"; then
  echo "Messenger2 missing /v1/messages; fallback to messenger1 for this run: $MESSENGER_URL2 -> $MESSENGER_URL"
  MESSENGER_URL2="$MESSENGER_URL"
  export MESSENGER_URL2
fi

if requires_public_key_header "$MESSENGER_URL2"; then
  echo "Messenger2 API mismatch at $MESSENGER_URL2: server requires X-Public-Key, but current Python client uses X-Agent-ID."
  exit 1
fi

cd "$ROOT_DIR/clients/python"

rm -f /tmp/t1.pub /tmp/t1.priv /tmp/t2.pub /tmp/t2.priv session_keys_*.json

python3 opennexus.py generate-keys --pub-key /tmp/t1.pub --priv-key /tmp/t1.priv
T1=$(cat /tmp/t1.pub)
python3 opennexus.py generate-keys --pub-key /tmp/t2.pub --priv-key /tmp/t2.priv
T2=$(cat /tmp/t2.pub)

T1_HEX=$(python3 -c "import base64,hashlib; print(hashlib.sha256(base64.b64decode('$T1')).digest()[:8].hex())")
T2_HEX=$(python3 -c "import base64,hashlib; print(hashlib.sha256(base64.b64decode('$T2')).digest()[:8].hex())")

echo "T1: ${T1:0:16} (session: session_keys_${T1_HEX}.json)"
echo "T2: ${T2:0:16} (session: session_keys_${T2_HEX}.json)"

echo ""
echo "=== Phase 1: T1 (messenger1) and T2 (messenger2) exchange messages ==="
PYTHONUNBUFFERED=1 MESSENGER_URL="$MESSENGER_URL" python3 opennexus.py stream --pub-key /tmp/t1.pub --priv-key /tmp/t1.priv > /tmp/t1_phase1.log 2>&1 &
RECV1_PID=$!
PYTHONUNBUFFERED=1 MESSENGER_URL="$MESSENGER_URL2" python3 opennexus.py stream --pub-key /tmp/t2.pub --priv-key /tmp/t2.priv > /tmp/t2_phase1.log 2>&1 &
RECV2_PID=$!
sleep 3

MESSENGER_URL="$MESSENGER_URL" python3 opennexus.py send --pub-key /tmp/t1.pub --priv-key /tmp/t1.priv --to "$T2" --messenger-url "$MESSENGER_URL2" --message "T1 to T2 - Message 1"
sleep 2

MESSENGER_URL="$MESSENGER_URL2" python3 opennexus.py send --pub-key /tmp/t2.pub --priv-key /tmp/t2.priv --to "$T1" --messenger-url "$MESSENGER_URL" --message "T2 to T1 - Message 1"
sleep 2

MESSENGER_URL="$MESSENGER_URL" python3 opennexus.py send --pub-key /tmp/t1.pub --priv-key /tmp/t1.priv --to "$T2" --messenger-url "$MESSENGER_URL2" --message "T1 to T2 - Message 2"
sleep 2

echo "Phase 1 assertions:"
check "T2 received Message 1" /tmp/t2_phase1.log "T1 to T2 - Message 1"
check "T1 received Message 1" /tmp/t1_phase1.log "T2 to T1 - Message 1"
check "T2 received Message 2" /tmp/t2_phase1.log "T1 to T2 - Message 2"
check "T2 sent HELLO_ACK" /tmp/t2_phase1.log "HELLO_ACK sent"

echo ""
echo "=== Phase 2: T2 restarts (loses key) ==="
kill "$RECV2_PID" 2>/dev/null || true
sleep 1
rm -f "session_keys_${T2_HEX}.json"

PYTHONUNBUFFERED=1 MESSENGER_URL="$MESSENGER_URL2" python3 opennexus.py stream --pub-key /tmp/t2.pub --priv-key /tmp/t2.priv > /tmp/t2_restart1.log 2>&1 &
RECV2_PID=$!
sleep 3

echo ""
echo "=== Phase 3: T1 sends (T2 sends RESET) ==="
MESSENGER_URL="$MESSENGER_URL" python3 opennexus.py send --pub-key /tmp/t1.pub --priv-key /tmp/t1.priv --to "$T2" --messenger-url "$MESSENGER_URL2" --message "T1 to T2 - Message 3 (after T2 restart)"
sleep 3

echo "Phase 3 assertions:"
check "T2 sent RESET" /tmp/t2_restart1.log "RESET"

echo ""
echo "=== Phase 4: T1 receives RESET ==="
sleep 3

echo "Phase 4 assertions:"
check "T1 received RESET" /tmp/t1_phase1.log "RESET"

echo ""
echo "=== Phase 5: T1 sends again (re-handshake) ==="
MESSENGER_URL="$MESSENGER_URL" python3 opennexus.py send --pub-key /tmp/t1.pub --priv-key /tmp/t1.priv --to "$T2" --messenger-url "$MESSENGER_URL2" --message "T1 to T2 - Message 4 (after reset)"
sleep 3

echo "Phase 5 assertions:"
check "T2 received Message 4" /tmp/t2_restart1.log "T1 to T2 - Message 4"

echo ""
echo "=== Phase 6: T2 restarts again ==="
kill "$RECV2_PID" 2>/dev/null || true
sleep 1
rm -f "session_keys_${T2_HEX}.json"

PYTHONUNBUFFERED=1 MESSENGER_URL="$MESSENGER_URL2" python3 opennexus.py stream --pub-key /tmp/t2.pub --priv-key /tmp/t2.priv > /tmp/t2_restart2.log 2>&1 &
RECV2_PID=$!
sleep 3

echo ""
echo "=== Phase 7: T2 sends (T1 sends RESET) ==="
MESSENGER_URL="$MESSENGER_URL2" python3 opennexus.py send --pub-key /tmp/t2.pub --priv-key /tmp/t2.priv --to "$T1" --messenger-url "$MESSENGER_URL" --message "T2 to T1 - Message 2 (after T2 restart)"
sleep 3

echo ""
echo "=== Phase 8: T2 receives RESET ==="
sleep 3

echo ""
echo "=== Phase 9: T2 sends again (re-handshake) ==="
MESSENGER_URL="$MESSENGER_URL2" python3 opennexus.py send --pub-key /tmp/t2.pub --priv-key /tmp/t2.priv --to "$T1" --messenger-url "$MESSENGER_URL" --message "T2 to T1 - Message 3 (after reset)"
sleep 3

echo "Phase 9 assertions:"
check "T1 received Message 3" /tmp/t1_phase1.log "T2 to T1 - Message 3"

rm -f "session_keys_${T1_HEX}.json" "session_keys_${T2_HEX}.json"

echo ""
echo "=========================================="
echo "RESULTS: $PASS passed, $FAIL failed"
echo "=========================================="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
echo "All tests passed!"