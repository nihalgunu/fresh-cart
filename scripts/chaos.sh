#!/bin/bash
set -o pipefail

GATEWAY="http://localhost:8000"
USER_SVC="http://localhost:8081"
PRODUCT_SVC="http://localhost:8082"
ORDER_SVC="http://localhost:8083"
NOTIF_SVC="http://localhost:8084"

PASS=0
FAIL=0
CORRELATION_ID="chaos-test-$(date +%s)"

green() { echo -e "\033[32m  PASS: $1\033[0m"; PASS=$((PASS+1)); }
red() { echo -e "\033[31m  FAIL: $1\033[0m"; FAIL=$((FAIL+1)); }
check() {
  if [ "$1" = "true" ]; then green "$2"; else red "$2"; fi
}

# Helper: wait for a service to respond with 200
wait_for_health() {
  local url="$1"
  local max_attempts="${2:-30}"
  local attempt=0

  while [ $attempt -lt $max_attempts ]; do
    STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$url" 2>/dev/null)
    if [ "$STATUS" = "200" ]; then
      return 0
    fi
    sleep 1
    attempt=$((attempt+1))
  done
  return 1
}

echo "============================================"
echo "  FreshCart Chaos Testing"
echo "  Correlation ID: $CORRELATION_ID"
echo "============================================"
echo ""

# ---- SEED TEST DATA ----
echo "--- Seeding Test Data ---"

# Register test user
REG_RESPONSE=$(curl -s -X POST "$GATEWAY/api/v1/auth/register" \
  -H "Content-Type: application/json" \
  -H "X-Correlation-ID: $CORRELATION_ID" \
  -d "{\"email\":\"chaos-$CORRELATION_ID@test.com\",\"password\":\"password123\",\"name\":\"Chaos User\",\"delivery_address\":\"123 Chaos St\"}")
TOKEN=$(echo "$REG_RESPONSE" | jq -r '.token // empty' 2>/dev/null)
USER_ID=$(echo "$REG_RESPONSE" | jq -r '.id // empty' 2>/dev/null)

if [ -n "$TOKEN" ] && [ -n "$USER_ID" ]; then
  echo "  User registered: $USER_ID"
else
  echo "  FATAL: Could not register test user"
  exit 1
fi

# Create test product with stock 1000
PRODUCT_RESPONSE=$(curl -s -X POST "$GATEWAY/api/v1/products" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Correlation-ID: $CORRELATION_ID" \
  -d '{"name":"Chaos Test Product","description":"For chaos testing","price":9.99,"category":"produce","stock":1000}')
PRODUCT_ID=$(echo "$PRODUCT_RESPONSE" | jq -r '.id // empty' 2>/dev/null)

if [ -n "$PRODUCT_ID" ]; then
  echo "  Product created: $PRODUCT_ID (stock: 1000)"
else
  echo "  FATAL: Could not create test product"
  exit 1
fi

echo ""

# ============================================================================
# SCENARIO 1: Kill product-service
# ============================================================================
echo "--- Scenario 1: Kill product-service ---"

# Stop product-service
docker compose stop product-service >/dev/null 2>&1
sleep 2

# Verify orders fail gracefully (not crash) - should get error response, not connection refused crash
ORDER_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY/api/v1/orders" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"123 Test St\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":1}]}" 2>/dev/null)
# Gateway should return an error code (5xx) but not crash
check "$([ "$ORDER_CODE" != "000" ] && echo true)" "Order fails gracefully (not crash) → $ORDER_CODE"

# Gateway should still be healthy
GW_HEALTH=$(curl -s -o /dev/null -w "%{http_code}" "$GATEWAY/health" 2>/dev/null)
check "$([ "$GW_HEALTH" = "200" ] && echo true)" "Gateway still healthy → $GW_HEALTH"

# Restart product-service
docker compose start product-service >/dev/null 2>&1
echo "  Restarting product-service..."
wait_for_health "$PRODUCT_SVC/health" 60
check "$([ $? -eq 0 ] && echo true)" "product-service recovered"

# Verify orders work again
sleep 2
ORDER_RESPONSE=$(curl -s -w "\n%{http_code}" -X POST "$GATEWAY/api/v1/orders" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Correlation-ID: $CORRELATION_ID" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"123 Test St\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":1}]}")
ORDER_CODE=$(echo "$ORDER_RESPONSE" | tail -1)
check "$([ "$ORDER_CODE" = "201" ] || [ "$ORDER_CODE" = "200" ] && echo true)" "Orders work after recovery → $ORDER_CODE"

echo ""

# ============================================================================
# SCENARIO 2: Kill user-db
# ============================================================================
echo "--- Scenario 2: Kill user-db ---"

# Stop user-db
docker compose stop user-db >/dev/null 2>&1
sleep 3

# Verify /ready returns 503
READY_CODE=$(curl -s -o /dev/null -w "%{http_code}" "$USER_SVC/ready" 2>/dev/null)
check "$([ "$READY_CODE" = "503" ] && echo true)" "user-service /ready returns 503 → $READY_CODE"

# Verify /health still returns 200
HEALTH_CODE=$(curl -s -o /dev/null -w "%{http_code}" "$USER_SVC/health" 2>/dev/null)
check "$([ "$HEALTH_CODE" = "200" ] && echo true)" "user-service /health still returns 200 → $HEALTH_CODE"

# Restart user-db
docker compose start user-db >/dev/null 2>&1
echo "  Restarting user-db..."
sleep 5

# Wait for /ready to recover
RETRIES=0
READY_RECOVERED=false
while [ $RETRIES -lt 30 ]; do
  READY_CODE=$(curl -s -o /dev/null -w "%{http_code}" "$USER_SVC/ready" 2>/dev/null)
  if [ "$READY_CODE" = "200" ]; then
    READY_RECOVERED=true
    break
  fi
  sleep 1
  RETRIES=$((RETRIES+1))
done
check "$([ "$READY_RECOVERED" = "true" ] && echo true)" "/ready recovers to 200"

echo ""

# ============================================================================
# SCENARIO 3: Kill rabbitmq
# ============================================================================
echo "--- Scenario 3: Kill rabbitmq ---"

# Stop rabbitmq
docker compose stop rabbitmq >/dev/null 2>&1
sleep 2

# Verify gateway still responds
GW_CODE=$(curl -s -o /dev/null -w "%{http_code}" "$GATEWAY/health" 2>/dev/null)
check "$([ "$GW_CODE" = "200" ] && echo true)" "Gateway still responds without rabbitmq → $GW_CODE"

# Restart rabbitmq
docker compose start rabbitmq >/dev/null 2>&1
echo "  Restarting rabbitmq..."

# Wait for rabbitmq to be healthy
RETRIES=0
RABBIT_READY=false
while [ $RETRIES -lt 60 ]; do
  RABBIT_CODE=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:15672/" 2>/dev/null)
  if [ "$RABBIT_CODE" = "200" ] || [ "$RABBIT_CODE" = "301" ]; then
    RABBIT_READY=true
    break
  fi
  sleep 2
  RETRIES=$((RETRIES+1))
done
check "$([ "$RABBIT_READY" = "true" ] && echo true)" "rabbitmq recovered"

# Give services time to reconnect
sleep 5

echo ""

# ============================================================================
# SCENARIO 4: Kill notification-service
# ============================================================================
echo "--- Scenario 4: Kill notification-service ---"

# Stop notification-service
docker compose stop notification-service >/dev/null 2>&1
sleep 2

# Verify orders still succeed without notification-service
ORDER_RESPONSE=$(curl -s -w "\n%{http_code}" -X POST "$GATEWAY/api/v1/orders" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Correlation-ID: $CORRELATION_ID" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"123 Test St\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":1}]}")
ORDER_CODE=$(echo "$ORDER_RESPONSE" | tail -1)
check "$([ "$ORDER_CODE" = "201" ] || [ "$ORDER_CODE" = "200" ] && echo true)" "Orders succeed without notification-service → $ORDER_CODE"

# Restart notification-service
docker compose start notification-service >/dev/null 2>&1
echo "  Restarting notification-service..."
wait_for_health "$NOTIF_SVC/health" 60
check "$([ $? -eq 0 ] && echo true)" "notification-service recovered"

echo ""

# ============================================================================
# SCENARIO 5: Flood gateway with 100 concurrent requests
# ============================================================================
echo "--- Scenario 5: Flood gateway with 100 concurrent requests ---"

# Create temp file for response codes
RESPONSE_FILE=$(mktemp)

# Send 100 concurrent requests
for i in $(seq 1 100); do
  curl -s -o /dev/null -w "%{http_code}\n" "$GATEWAY/health" >> "$RESPONSE_FILE" &
done
wait

# Count response codes
TOTAL=$(wc -l < "$RESPONSE_FILE" | tr -d ' ')
OK_COUNT=$(grep -c "^200$" "$RESPONSE_FILE" 2>/dev/null || echo 0)
RATE_LIMITED=$(grep -c "^429$" "$RESPONSE_FILE" 2>/dev/null || echo 0)
ERROR_COUNT=$(grep -vE "^(200|429)$" "$RESPONSE_FILE" | wc -l | tr -d ' ')

echo "  Responses: 200=$OK_COUNT, 429=$RATE_LIMITED, errors=$ERROR_COUNT"
check "$([ "$ERROR_COUNT" -eq 0 ] && echo true)" "No errors (only 200s and 429s) → errors: $ERROR_COUNT"

rm -f "$RESPONSE_FILE"

echo ""

# ============================================================================
# FINAL VERIFICATION
# ============================================================================
echo "--- Final Verification: All Services Healthy ---"

# Check all 5 services
for svc in "$GATEWAY" "$USER_SVC" "$PRODUCT_SVC" "$ORDER_SVC" "$NOTIF_SVC"; do
  STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$svc/health" 2>/dev/null)
  check "$([ "$STATUS" = "200" ] && echo true)" "Health $svc → $STATUS"
done

echo ""
echo "--- Final End-to-End Order ---"

# Place final order
FINAL_ORDER=$(curl -s -w "\n%{http_code}" -X POST "$GATEWAY/api/v1/orders" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Correlation-ID: $CORRELATION_ID-final" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"123 Final Test St\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":1}]}")
FINAL_CODE=$(echo "$FINAL_ORDER" | tail -1)
FINAL_STATUS=$(echo "$FINAL_ORDER" | head -1 | jq -r '.status // empty' 2>/dev/null)
check "$([ "$FINAL_CODE" = "201" ] || [ "$FINAL_CODE" = "200" ] && echo true)" "Final order placed → $FINAL_CODE"
check "$([ "$FINAL_STATUS" = "confirmed" ] && echo true)" "Final order confirmed → $FINAL_STATUS"

# ---- SUMMARY ----
echo ""
echo "============================================"
TOTAL=$((PASS + FAIL))
echo "  Results: $PASS/$TOTAL passed"
if [ "$FAIL" -gt 0 ]; then
  echo -e "  \033[31m$FAIL FAILED\033[0m"
  exit 1
else
  echo -e "  \033[32mALL PASSED\033[0m"
  exit 0
fi
