#!/bin/bash
# Example test script for process management API
# Make sure to set SANDBOX_SECRET environment variable before running

if [ -z "$SANDBOX_SECRET" ]; then
    echo "Error: SANDBOX_SECRET environment variable not set"
    exit 1
fi

BASE_URL="${BASE_URL:-http://localhost:3030}"

echo "=== Testing Process Management API ==="
echo

# Test 1: Start a process
echo "1. Starting a long-running process..."
START_RESPONSE=$(curl -s -X POST "$BASE_URL/start_process" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d '{
    "cmd": "bash -c \"for i in {1..10}; do echo Line $i; sleep 1; done\"",
    "env": {
      "TEST_VAR": "test_value"
    }
  }')

echo "Response: $START_RESPONSE"
PROCESS_ID=$(echo "$START_RESPONSE" | jq -r '.id')
echo "Process ID: $PROCESS_ID"
echo

# Test 2: List processes
echo "2. Listing all processes..."
curl -s -X GET "$BASE_URL/list_processes" -H "Authorization: Bearer $SANDBOX_SECRET"
echo

# Test 3: Start streaming logs (in background)
echo "3. Streaming process logs (for 5 seconds)..."
curl -s -X GET "$BASE_URL/process_logs_streaming?id=$PROCESS_ID" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  --no-buffer &
STREAM_PID=$!

# Let it stream for a bit
sleep 5

# Stop streaming
kill $STREAM_PID 2>/dev/null
echo
echo

# Test 4: Kill the process
echo "4. Killing the process..."
curl -s -X POST "$BASE_URL/kill_process" \
  -H "Authorization: Bearer $SANDBOX_SECRET" \
  -H "Content-Type: application/json" \
  -d "{\"id\": \"$PROCESS_ID\"}" | jq
echo

# Test 5: List processes again to see the killed status
echo "5. Listing processes after kill..."
sleep 1  # Give it a moment to update status
curl -s -X GET "$BASE_URL/list_processes" \
  -H "Authorization: Bearer $SANDBOX_SECRET" | jq
echo

echo "=== Tests Complete ==="
