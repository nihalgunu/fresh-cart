#!/bin/bash
set -o pipefail

GATEWAY="http://localhost:8000"

echo "============================================"
echo "  FreshCart Load Testing"
echo "  Gateway: $GATEWAY"
echo "============================================"
echo ""

# ---- SEED TEST DATA ----
echo "--- Seeding Test Data ---"

CORRELATION_ID="loadtest-$(date +%s)"

# Register test user
REG_RESPONSE=$(curl -s -X POST "$GATEWAY/api/v1/auth/register" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"loadtest-$CORRELATION_ID@test.com\",\"password\":\"password123\",\"name\":\"Load Test User\",\"delivery_address\":\"123 Load Test St\"}")
TOKEN=$(echo "$REG_RESPONSE" | jq -r '.token // empty' 2>/dev/null)
USER_ID=$(echo "$REG_RESPONSE" | jq -r '.id // empty' 2>/dev/null)

if [ -n "$TOKEN" ] && [ -n "$USER_ID" ]; then
  echo "  User registered: $USER_ID"
else
  echo "  FATAL: Could not register test user"
  exit 1
fi

# Create test product with high stock for order testing
PRODUCT_RESPONSE=$(curl -s -X POST "$GATEWAY/api/v1/products" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"name":"Load Test Product","description":"For load testing","price":9.99,"category":"produce","stock":10000}')
PRODUCT_ID=$(echo "$PRODUCT_RESPONSE" | jq -r '.id // empty' 2>/dev/null)

if [ -n "$PRODUCT_ID" ]; then
  echo "  Product created: $PRODUCT_ID (stock: 10000)"
else
  echo "  FATAL: Could not create test product"
  exit 1
fi

echo ""

# ---- HELPER FUNCTIONS ----

# Calculate percentile from sorted file
# Usage: percentile <file> <percentile>
percentile() {
  local file="$1"
  local pct="$2"
  local count=$(wc -l < "$file" | tr -d ' ')

  if [ "$count" -eq 0 ]; then
    echo "0"
    return
  fi

  # Calculate index (1-based for sed)
  local index=$(echo "scale=0; ($count * $pct / 100) + 1" | bc)
  if [ "$index" -gt "$count" ]; then
    index=$count
  fi

  sed -n "${index}p" "$file"
}

# Run load test scenario
# Usage: run_scenario <name> <concurrent> <total> <method> <url> <auth> <body>
run_scenario() {
  local name="$1"
  local concurrent="$2"
  local total="$3"
  local method="$4"
  local url="$5"
  local auth="$6"
  local body="$7"

  echo "--- Scenario: $name ---"
  echo "  Concurrent: $concurrent | Total: $total | $method $url"

  # Temp files for results
  local latency_file=$(mktemp)
  local status_file=$(mktemp)

  local start_time=$(date +%s.%N)
  local completed=0

  while [ $completed -lt $total ]; do
    # Calculate batch size
    local remaining=$((total - completed))
    local batch=$concurrent
    if [ $remaining -lt $batch ]; then
      batch=$remaining
    fi

    # Launch batch of concurrent requests
    for i in $(seq 1 $batch); do
      (
        if [ "$method" = "GET" ]; then
          if [ -n "$auth" ]; then
            result=$(curl -s -o /dev/null -w "%{http_code} %{time_total}" \
              -H "Authorization: Bearer $auth" "$url" 2>/dev/null)
          else
            result=$(curl -s -o /dev/null -w "%{http_code} %{time_total}" "$url" 2>/dev/null)
          fi
        else
          result=$(curl -s -o /dev/null -w "%{http_code} %{time_total}" \
            -X POST \
            -H "Content-Type: application/json" \
            -H "Authorization: Bearer $auth" \
            -d "$body" "$url" 2>/dev/null)
        fi

        local code=$(echo "$result" | awk '{print $1}')
        local time=$(echo "$result" | awk '{print $2}')

        echo "$code" >> "$status_file"
        echo "$time" >> "$latency_file"
      ) &
    done

    # Wait for batch to complete
    wait
    completed=$((completed + batch))
  done

  local end_time=$(date +%s.%N)
  local total_time=$(echo "$end_time - $start_time" | bc)

  # Calculate statistics
  local success=$(grep -cE "^(200|201)$" "$status_file" 2>/dev/null || echo 0)
  local rate_limited=$(grep -c "^429$" "$status_file" 2>/dev/null || echo 0)
  local total_responses=$(wc -l < "$status_file" | tr -d ' ')
  local errors=$((total_responses - success - rate_limited))

  local throughput=$(echo "scale=2; $total_responses / $total_time" | bc)

  # Sort latencies for percentile calculation
  local sorted_latency=$(mktemp)
  sort -n "$latency_file" > "$sorted_latency"

  local p50=$(percentile "$sorted_latency" 50)
  local p95=$(percentile "$sorted_latency" 95)
  local p99=$(percentile "$sorted_latency" 99)

  # Print results
  echo ""
  echo "  Results:"
  echo "    Total time:    ${total_time}s"
  echo "    Throughput:    ${throughput} req/s"
  echo "    Success:       $success"
  echo "    Rate limited:  $rate_limited"
  echo "    Errors:        $errors"
  echo "    Latency p50:   ${p50}s"
  echo "    Latency p95:   ${p95}s"
  echo "    Latency p99:   ${p99}s"
  echo ""

  # Cleanup
  rm -f "$latency_file" "$status_file" "$sorted_latency"
}

# ============================================================================
# SCENARIO 1: Sustained Load - Health Endpoint
# ============================================================================
run_scenario \
  "Sustained Load (Health)" \
  10 \
  200 \
  "GET" \
  "$GATEWAY/health" \
  "" \
  ""

# ============================================================================
# SCENARIO 2: Spike Load - Products Endpoint
# ============================================================================
run_scenario \
  "Spike Load (Products)" \
  50 \
  200 \
  "GET" \
  "$GATEWAY/api/v1/products" \
  "$TOKEN" \
  ""

# ============================================================================
# SCENARIO 3: Order Placement
# ============================================================================
ORDER_BODY="{\"user_id\":\"$USER_ID\",\"delivery_address\":\"123 Load Test St\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":1}]}"

run_scenario \
  "Order Placement" \
  10 \
  50 \
  "POST" \
  "$GATEWAY/api/v1/orders" \
  "$TOKEN" \
  "$ORDER_BODY"

# ---- SUMMARY ----
echo "============================================"
echo "  Load Testing Complete"
echo "============================================"
