#!/bin/bash
set -o pipefail

# =============================================================================
# FreshCart Load Testing (Kubernetes)
# =============================================================================
# Scenarios:
#   1. Sustained load - steady 10 req/s for 30 seconds
#   2. Spike load - burst of 50 concurrent requests
#   3. Soak test - moderate load for 60 seconds
#
# Output: p50/p95/p99 latency percentiles
# =============================================================================

GATEWAY_URL="http://localhost:30080"

echo "============================================"
echo "  FreshCart Load Testing (Kubernetes)"
echo "  Gateway: $GATEWAY_URL"
echo "============================================"
echo ""

# Pre-flight check
GW_STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$GATEWAY_URL/health" 2>/dev/null)
if [ "$GW_STATUS" != "200" ]; then
  echo "FATAL: Gateway not accessible at $GATEWAY_URL"
  echo "Run: make boot"
  exit 1
fi

# ---- SEED DATA ----
echo "--- Seeding Test Data ---"
CORRELATION_ID="loadtest-$(date +%s)"

REG=$(curl -s -X POST "$GATEWAY_URL/api/v1/auth/register" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"load-$CORRELATION_ID@test.com\",\"password\":\"password123\",\"name\":\"Load\",\"delivery_address\":\"123 St\"}")
TOKEN=$(echo "$REG" | jq -r '.token // empty')
USER_ID=$(echo "$REG" | jq -r '.id // empty')
[ -z "$TOKEN" ] && echo "FATAL: Could not register user" && exit 1
echo "  User: $USER_ID"

PROD=$(curl -s -X POST "$GATEWAY_URL/api/v1/products" \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d '{"name":"Load Product","description":"Test","price":9.99,"category":"produce","stock":100000}')
PRODUCT_ID=$(echo "$PROD" | jq -r '.id // empty')
[ -z "$PRODUCT_ID" ] && echo "FATAL: Could not create product" && exit 1
echo "  Product: $PRODUCT_ID (stock: 100000)"
echo ""

# ---- HELPER FUNCTIONS ----
percentile() {
  local file="$1" pct="$2"
  local count=$(wc -l < "$file" | tr -d ' ')
  [ "$count" -eq 0 ] && echo "0" && return
  local idx=$(echo "scale=0; ($count * $pct / 100) + 1" | bc)
  [ "$idx" -gt "$count" ] && idx=$count
  sed -n "${idx}p" "$file"
}

print_stats() {
  local lat_file="$1" status_file="$2" duration="$3"

  local total=$(wc -l < "$status_file" | tr -d ' \n')
  local ok=$(grep -cE "^(200|201)$" "$status_file" 2>/dev/null | tr -d ' \n' || echo 0)
  local r429=$(grep -c "^429$" "$status_file" 2>/dev/null | tr -d ' \n' || echo 0)
  [ -z "$ok" ] && ok=0
  [ -z "$r429" ] && r429=0
  [ -z "$total" ] && total=0
  local err=$((total - ok - r429))
  local throughput=$(echo "scale=2; $total / $duration" | bc 2>/dev/null || echo 0)

  local sorted=$(mktemp)
  sort -n "$lat_file" > "$sorted"
  local p50=$(percentile "$sorted" 50)
  local p95=$(percentile "$sorted" 95)
  local p99=$(percentile "$sorted" 99)
  rm -f "$sorted"

  echo "  Duration:   ${duration}s"
  echo "  Requests:   $total"
  echo "  Throughput: ${throughput} req/s"
  echo "  Success:    $ok ($(echo "scale=1; $ok * 100 / $total" | bc)%)"
  echo "  Rate-limit: $r429"
  echo "  Errors:     $err"
  echo ""
  echo "  Latency:"
  echo "    p50: ${p50}s"
  echo "    p95: ${p95}s"
  echo "    p99: ${p99}s"
}

# ============================================================================
# SCENARIO 1: Sustained Load (10 req/s for 30 seconds = 300 requests)
# ============================================================================
echo "============================================"
echo "  Scenario 1: SUSTAINED LOAD"
echo "  10 requests/second for 30 seconds"
echo "============================================"

LAT=$(mktemp)
STATUS=$(mktemp)
START=$(date +%s)

for i in $(seq 1 30); do
  for j in $(seq 1 10); do
    (
      r=$(curl -s -o /dev/null -w "%{http_code} %{time_total}" "$GATEWAY_URL/health" 2>/dev/null)
      echo "$r" | awk '{print $1}' >> "$STATUS"
      echo "$r" | awk '{print $2}' >> "$LAT"
    ) &
  done
  sleep 1
done
wait

END=$(date +%s)
DURATION=$((END - START))
print_stats "$LAT" "$STATUS" "$DURATION"
rm -f "$LAT" "$STATUS"
echo ""

# ============================================================================
# SCENARIO 2: Spike Load (50 concurrent, 3 bursts)
# ============================================================================
echo "============================================"
echo "  Scenario 2: SPIKE LOAD"
echo "  50 concurrent requests x 3 bursts"
echo "============================================"

LAT=$(mktemp)
STATUS=$(mktemp)
START=$(date +%s)

for burst in 1 2 3; do
  echo "  Burst $burst..."
  for i in $(seq 1 50); do
    (
      r=$(curl -s -o /dev/null -w "%{http_code} %{time_total}" \
        "$GATEWAY_URL/api/v1/products" -H "Authorization: Bearer $TOKEN" 2>/dev/null)
      echo "$r" | awk '{print $1}' >> "$STATUS"
      echo "$r" | awk '{print $2}' >> "$LAT"
    ) &
  done
  wait
  sleep 2
done

END=$(date +%s)
DURATION=$((END - START))
print_stats "$LAT" "$STATUS" "$DURATION"
rm -f "$LAT" "$STATUS"
echo ""

# ============================================================================
# SCENARIO 3: Soak Test (5 req/s for 60 seconds = 300 requests)
# ============================================================================
echo "============================================"
echo "  Scenario 3: SOAK TEST"
echo "  5 requests/second for 60 seconds"
echo "============================================"

LAT=$(mktemp)
STATUS=$(mktemp)
START=$(date +%s)

ORDER_BODY="{\"user_id\":\"$USER_ID\",\"delivery_address\":\"Load Test\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":1}]}"

for i in $(seq 1 60); do
  for j in $(seq 1 5); do
    (
      r=$(curl -s -o /dev/null -w "%{http_code} %{time_total}" \
        -X POST "$GATEWAY_URL/api/v1/orders" \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $TOKEN" \
        -d "$ORDER_BODY" 2>/dev/null)
      echo "$r" | awk '{print $1}' >> "$STATUS"
      echo "$r" | awk '{print $2}' >> "$LAT"
    ) &
  done
  sleep 1
  [ $((i % 10)) -eq 0 ] && echo "  ${i}s elapsed..."
done
wait

END=$(date +%s)
DURATION=$((END - START))
print_stats "$LAT" "$STATUS" "$DURATION"
rm -f "$LAT" "$STATUS"
echo ""

echo "============================================"
echo "  Load Testing Complete"
echo "============================================"
