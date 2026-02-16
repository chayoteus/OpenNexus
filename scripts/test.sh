#!/bin/bash
set -e

MESSENGER_URL="${MESSENGER_URL:-http://localhost:8080}"
MESSENGER_URL2="${MESSENGER_URL2:-http://localhost:8081}"
export MESSENGER_URL MESSENGER_URL2

echo "T1 messenger: $MESSENGER_URL"
echo "T2 messenger: $MESSENGER_URL2"

PASS=0
FAIL=0

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

cd "$(dirname "$0")/../clients/python"

rm -f /tmp/t1.pub /tmp/t1.priv /tmp/t2.pub /tmp/t2.priv session_keys_*.json

# Generate keys
python3 opennexus.py generate-keys --pub-key /tmp/t1.pub --priv-key /tmp/t1.priv
T1=$(cat /tmp/t1.pub)
python3 opennexus.py generate-keys --pub-key /tmp/t2.pub --priv-key /tmp/t2.priv
T2=$(cat /tmp/t2.pub)

# Compute hex session key filenames (must match Python: sha256(pubkey)[:8].hex())
T1_HEX=$(python3 -c "import base64,hashlib; print(hashlib.sha256(base64.b64decode('$T1')).digest()[:8].hex())")
T2_HEX=$(python3 -c "import base64,hashlib; print(hashlib.sha256(base64.b64decode('$T2')).digest()[:8].hex())")

echo "T1: ${T1:0:16} (session: session_keys_${T1_HEX}.json)"
echo "T2: ${T2:0:16} (session: session_keys_${T2_HEX}.json)"

# ===== Phase 1: T1 on messenger1, T2 on messenger2, exchange messages =====
echo ""
echo "=== Phase 1: T1 (messenger1) and T2 (messenger2) exchange messages ==="
MESSENGER_URL="$MESSENGER_URL" python3 opennexus.py stream --pub-key /tmp/t1.pub --priv-key /tmp/t1.priv > /tmp/t1_phase1.log 2>&1 &
RECV1_PID=$!
MESSENGER_URL="$MESSENGER_URL2" python3 opennexus.py stream --pub-key /tmp/t2.pub --priv-key /tmp/t2.priv > /tmp/t2_phase1.log 2>&1 &
RECV2_PID=$!
sleep 3

# T1 sends to T2 (T1 posts to T2's messenger, T1's own URL is MESSENGER_URL)
MESSENGER_URL="$MESSENGER_URL" python3 opennexus.py send --pub-key /tmp/t1.pub --priv-key /tmp/t1.priv --to "$T2" --messenger-url "$MESSENGER_URL2" --message "T1 to T2 - Message 1"
sleep 2

# T2 sends to T1 (T2 posts to T1's messenger, T2's own URL is MESSENGER_URL2)
MESSENGER_URL="$MESSENGER_URL2" python3 opennexus.py send --pub-key /tmp/t2.pub --priv-key /tmp/t2.priv --to "$T1" --messenger-url "$MESSENGER_URL" --message "T2 to T1 - Message 1"
sleep 2

# T1 sends to T2 again (cached key)
MESSENGER_URL="$MESSENGER_URL" python3 opennexus.py send --pub-key /tmp/t1.pub --priv-key /tmp/t1.priv --to "$T2" --messenger-url "$MESSENGER_URL2" --message "T1 to T2 - Message 2"
sleep 2

echo "Phase 1 assertions:"
check "T2 received Message 1" /tmp/t2_phase1.log "T1 to T2 - Message 1"
check "T1 received Message 1" /tmp/t1_phase1.log "T2 to T1 - Message 1"
check "T2 received Message 2" /tmp/t2_phase1.log "T1 to T2 - Message 2"
check "T2 sent HELLO_ACK" /tmp/t2_phase1.log "HELLO_ACK sent"

# ===== Phase 2: T2 restarts (loses key) =====
echo ""
echo "=== Phase 2: T2 restarts (loses key) ==="
kill $RECV2_PID 2>/dev/null
sleep 1
rm -f "session_keys_${T2_HEX}.json"

MESSENGER_URL="$MESSENGER_URL2" python3 opennexus.py stream --pub-key /tmp/t2.pub --priv-key /tmp/t2.priv > /tmp/t2_restart1.log 2>&1 &
RECV2_PID=$!
sleep 3

# ===== Phase 3: T1 sends to T2 (T2 can't decrypt -> RESET) =====
echo ""
echo "=== Phase 3: T1 sends (T2 sends RESET) ==="
MESSENGER_URL="$MESSENGER_URL" python3 opennexus.py send --pub-key /tmp/t1.pub --priv-key /tmp/t1.priv --to "$T2" --messenger-url "$MESSENGER_URL2" --message "T1 to T2 - Message 3 (after T2 restart)"
sleep 3

echo "Phase 3 assertions:"
check "T2 sent RESET" /tmp/t2_restart1.log "RESET"

# ===== Phase 4: T1 receives RESET =====
echo ""
echo "=== Phase 4: T1 receives RESET ==="
sleep 3

echo "Phase 4 assertions:"
check "T1 received RESET" /tmp/t1_phase1.log "RESET"

# ===== Phase 5: T1 sends again (should re-handshake) =====
echo ""
echo "=== Phase 5: T1 sends again (re-handshake) ==="
MESSENGER_URL="$MESSENGER_URL" python3 opennexus.py send --pub-key /tmp/t1.pub --priv-key /tmp/t1.priv --to "$T2" --messenger-url "$MESSENGER_URL2" --message "T1 to T2 - Message 4 (after reset)"
sleep 3

echo "Phase 5 assertions:"
check "T2 received Message 4" /tmp/t2_restart1.log "T1 to T2 - Message 4"

# ===== Phase 6: T2 restarts again =====
echo ""
echo "=== Phase 6: T2 restarts again ==="
kill $RECV2_PID 2>/dev/null
sleep 1
rm -f "session_keys_${T2_HEX}.json"

MESSENGER_URL="$MESSENGER_URL2" python3 opennexus.py stream --pub-key /tmp/t2.pub --priv-key /tmp/t2.priv > /tmp/t2_restart2.log 2>&1 &
RECV2_PID=$!
sleep 3

# ===== Phase 7: T2 sends to T1 =====
echo ""
echo "=== Phase 7: T2 sends (T1 sends RESET) ==="
MESSENGER_URL="$MESSENGER_URL2" python3 opennexus.py send --pub-key /tmp/t2.pub --priv-key /tmp/t2.priv --to "$T1" --messenger-url "$MESSENGER_URL" --message "T2 to T1 - Message 2 (after T2 restart)"
sleep 3

# ===== Phase 8: T2 receives RESET =====
echo ""
echo "=== Phase 8: T2 receives RESET ==="
sleep 3

# ===== Phase 9: T2 sends again (re-handshake) =====
echo ""
echo "=== Phase 9: T2 sends again (re-handshake) ==="
MESSENGER_URL="$MESSENGER_URL2" python3 opennexus.py send --pub-key /tmp/t2.pub --priv-key /tmp/t2.priv --to "$T1" --messenger-url "$MESSENGER_URL" --message "T2 to T1 - Message 3 (after reset)"
sleep 3

echo "Phase 9 assertions:"
check "T1 received Message 3" /tmp/t1_phase1.log "T2 to T1 - Message 3"

# ===== Results =====
kill $RECV1_PID $RECV2_PID 2>/dev/null
rm -f "session_keys_${T1_HEX}.json" "session_keys_${T2_HEX}.json" /tmp/t*.pub /tmp/t*.priv /tmp/t*.log

echo ""
echo "=========================================="
echo "RESULTS: $PASS passed, $FAIL failed"
echo "=========================================="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
echo "All tests passed!"
