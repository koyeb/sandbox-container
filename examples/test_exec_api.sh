#!/bin/bash
# Example test script for the exec (/run) and exec streaming (/run_streaming) APIs.
# Make sure to set SANDBOX_SECRET environment variable before running.
set -euo pipefail

if [ -z "$SANDBOX_SECRET" ]; then
    echo "Error: SANDBOX_SECRET environment variable not set"
    exit 1
fi

BASE_URL="${BASE_URL:-http://localhost:3030}"

echo "=== Testing Exec API ==="
echo

# --- /run examples ---

echo "1. Basic stdout capture"
curl -s -X POST "$BASE_URL/run" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "echo hello world"}' | jq
echo

echo "2. Stderr capture"
curl -s -X POST "$BASE_URL/run" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "echo error message >&2"}' | jq
echo

echo "3. Non-zero exit code"
curl -s -X POST "$BASE_URL/run" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "exit 42"}' | jq
echo

echo "4. Custom environment variable"
curl -s -X POST "$BASE_URL/run" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "echo $GREETING", "env": {"GREETING": "hello from env"}}' | jq
echo

echo "5. Custom working directory"
curl -s -X POST "$BASE_URL/run" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "pwd", "cwd": "/tmp"}' | jq
echo

echo "6. Mixed stdout and stderr"
curl -s -X POST "$BASE_URL/run" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "echo out; echo err >&2; echo out2"}' | jq
echo

# --- /run_streaming examples ---

echo "=== Testing Exec Streaming API ==="
echo

echo "7. Basic streaming stdout"
curl -s -X POST "$BASE_URL/run_streaming" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "echo hello world"}'
echo

echo "8. Streaming output over time"
curl -s -X POST "$BASE_URL/run_streaming" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "for i in 1 2 3; do echo line $i; sleep 0.2; done"}'
echo

echo "9. Streaming stderr"
curl -s -X POST "$BASE_URL/run_streaming" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "echo error >&2"}'
echo

echo "10. Streaming with non-zero exit code (check complete event)"
curl -s -X POST "$BASE_URL/run_streaming" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "echo done; exit 1"}'
echo

echo "11. Streaming with custom env and cwd"
curl -s -X POST "$BASE_URL/run_streaming" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "echo $MSG && pwd", "env": {"MSG": "hello"}, "cwd": "/tmp"}'
echo

echo "12. Large output via /run (100MB) — verifies stdout is fully received"
LARGE_SIZE=104857600  # 100MB
RESPONSE=$(curl -s -X POST "$BASE_URL/run" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d "{\"cmd\": \"head -c $LARGE_SIZE /dev/zero | tr '\\\\0' 'a'\"}")
RECEIVED=$(echo "$RESPONSE" | jq -r '.stdout' | wc -c)
# jq adds a trailing newline, subtract 1
RECEIVED=$((RECEIVED - 1))
if [ "$RECEIVED" -eq "$LARGE_SIZE" ]; then
  echo "OK: received $RECEIVED bytes"
else
  echo "FAIL: expected $LARGE_SIZE bytes, got $RECEIVED"
  exit 1
fi
echo

echo "13. Large output via /run_streaming (100MB) — verifies all chunks are streamed"
TMPFILE=$(mktemp)
curl -s -X POST "$BASE_URL/run_streaming" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d "{\"cmd\": \"head -c $LARGE_SIZE /dev/zero | tr '\\\\0' 'a'\"}" > "$TMPFILE"
# Each SSE data line is "data: <json>"; strip the prefix and extract the stdout payload.
# jq adds a trailing newline per output line, so subtract the event count.
EVENT_COUNT=$(grep -c '"stream":"stdout"' "$TMPFILE" || true)
RECEIVED=$(grep '^data: ' "$TMPFILE" \
  | sed 's/^data: //' \
  | jq -r 'select(.stream == "stdout") | .data' \
  | wc -c)
RECEIVED=$((RECEIVED - EVENT_COUNT))
rm "$TMPFILE"
if [ "$RECEIVED" -eq "$LARGE_SIZE" ]; then
  echo "OK: received $RECEIVED bytes across $EVENT_COUNT chunks"
else
  echo "FAIL: expected $LARGE_SIZE bytes, got $RECEIVED"
  exit 1
fi
echo

echo "=== Tests Complete ==="
