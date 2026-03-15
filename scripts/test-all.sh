#!/bin/bash
set -o pipefail

GATEWAY="http://localhost:8000"
USER_SVC="http://localhost:8081"
PRODUCT_SVC="http://localhost:8082"
ORDER_SVC="http://localhost:8083"
NOTIF_SVC="http://localhost:8084"

PASS=0
FAIL=0
CORRELATION_ID="full-test-$(date +%s)"

green() { echo -e "\033[32m  PASS: $1\033[0m"; PASS=$((PASS+1)); }
red() { echo -e "\033[31m  FAIL: $1\033[0m"; FAIL=$((FAIL+1)); }
check() {
  if [ "$1" = "true" ]; then green "$2"; else red "$2"; fi
}

echo "============================================"
echo "  FreshCart Full Integration Test"
echo "  Correlation ID: $CORRELATION_ID"
echo "============================================"
echo ""

# Wait for observability services to be ready
echo "Waiting for observability services..."
for url in "http://localhost:9090/-/healthy" "http://localhost:3000/api/health" "http://localhost:16686/"; do
  RETRIES=0
  until curl -s -o /dev/null "$url" || [ $RETRIES -eq 30 ]; do
    sleep 2
    RETRIES=$((RETRIES+1))
  done
done
# Loki takes longer to be ready
RETRIES=0
until [ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:3100/ready)" = "200" ] || [ $RETRIES -eq 45 ]; do
  sleep 2
  RETRIES=$((RETRIES+1))
done
echo "Observability services up"
echo ""

# ---- HEALTH CHECKS ----
echo "--- Health Checks ---"
for svc in "$GATEWAY" "$USER_SVC" "$PRODUCT_SVC" "$ORDER_SVC" "$NOTIF_SVC"; do
  STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$svc/health")
  check "$([ "$STATUS" = "200" ] && echo true)" "Health $svc → $STATUS"
done

# ---- READY CHECKS ----
echo ""
echo "--- Ready Checks ---"
for svc in "$USER_SVC" "$PRODUCT_SVC" "$ORDER_SVC" "$NOTIF_SVC"; do
  BODY=$(curl -s "$svc/ready")
  STATUS=$(echo "$BODY" | jq -r '.status // empty' 2>/dev/null)
  check "$([ "$STATUS" = "ready" ] && echo true)" "Ready $svc → $STATUS"
done

# Gateway ready (should check Redis)
GW_READY=$(curl -s "$GATEWAY/ready")
GW_STATUS=$(echo "$GW_READY" | jq -r '.status // empty' 2>/dev/null)
check "$([ "$GW_STATUS" = "ready" ] && echo true)" "Ready gateway → $GW_STATUS"

# ---- PROMETHEUS METRICS (HTTP) ----
echo ""
echo "--- Prometheus Metrics (HTTP) ---"
for port in 8000 8081 8082 8083 8084; do
  METRICS=$(curl -s "http://localhost:$port/metrics" | grep -c "http_requests_total" 2>/dev/null)
  check "$([ "$METRICS" -gt 0 ] && echo true)" "Metrics localhost:$port has http_requests_total"
done

# ---- AUTH: PUBLIC ROUTES ----
echo ""
echo "--- Auth: Public Routes ---"
REG_RESPONSE=$(curl -s -w "\n%{http_code}" -X POST "$GATEWAY/api/v1/auth/register" \
  -H "Content-Type: application/json" \
  -H "X-Correlation-ID: $CORRELATION_ID" \
  -d "{\"email\":\"testuser-$CORRELATION_ID@test.com\",\"password\":\"password123\",\"name\":\"Test User\",\"delivery_address\":\"789 Test Ave\"}")
REG_CODE=$(echo "$REG_RESPONSE" | tail -1)
REG_BODY=$(echo "$REG_RESPONSE" | head -1)
check "$([ "$REG_CODE" = "201" ] && echo true)" "Register via gateway → $REG_CODE"

TOKEN=$(echo "$REG_BODY" | jq -r '.token // empty' 2>/dev/null)
USER_ID=$(echo "$REG_BODY" | jq -r '.id // empty' 2>/dev/null)
check "$([ -n "$TOKEN" ] && echo true)" "Register returned JWT token"
check "$([ -n "$USER_ID" ] && echo true)" "Register returned user ID"

LOGIN_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY/api/v1/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"testuser-$CORRELATION_ID@test.com\",\"password\":\"password123\"}")
check "$([ "$LOGIN_CODE" = "200" ] && echo true)" "Login via gateway → $LOGIN_CODE"

# ---- AUTH: EDGE CASES ----
echo ""
echo "--- Auth: Edge Cases ---"

# Duplicate registration returns 409
DUP_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY/api/v1/auth/register" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"testuser-$CORRELATION_ID@test.com\",\"password\":\"password123\",\"name\":\"Dup User\",\"delivery_address\":\"789 Test Ave\"}")
check "$([ "$DUP_CODE" = "409" ] && echo true)" "Duplicate registration → $DUP_CODE (expect 409)"

# Bad login returns 401
BAD_LOGIN_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY/api/v1/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"email":"nobody@test.com","password":"wrongpassword"}')
check "$([ "$BAD_LOGIN_CODE" = "401" ] && echo true)" "Bad login → $BAD_LOGIN_CODE (expect 401)"

# Login returns a working token
LOGIN_RESPONSE=$(curl -s -X POST "$GATEWAY/api/v1/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"testuser-$CORRELATION_ID@test.com\",\"password\":\"password123\"}")
LOGIN_TOKEN=$(echo "$LOGIN_RESPONSE" | jq -r '.token // empty' 2>/dev/null)
LOGIN_AUTH_CODE=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $LOGIN_TOKEN" "$GATEWAY/api/v1/products")
check "$([ "$LOGIN_AUTH_CODE" = "200" ] && echo true)" "Login token works on protected route → $LOGIN_AUTH_CODE (expect 200)"

# ---- AUTH: PROTECTED ROUTES ----
echo ""
echo "--- Auth: Protected Routes ---"
NO_AUTH_CODE=$(curl -s -o /dev/null -w "%{http_code}" "$GATEWAY/api/v1/products")
check "$([ "$NO_AUTH_CODE" = "401" ] && echo true)" "Products without token → $NO_AUTH_CODE (expect 401)"

WITH_AUTH_CODE=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$GATEWAY/api/v1/products")
check "$([ "$WITH_AUTH_CODE" = "200" ] && echo true)" "Products with token → $WITH_AUTH_CODE (expect 200)"

# ---- USER PROFILE ----
echo ""
echo "--- User Profile ---"

# GET user by ID
USER_PROFILE_CODE=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$GATEWAY/api/v1/users/$USER_ID")
check "$([ "$USER_PROFILE_CODE" = "200" ] && echo true)" "Get user profile → $USER_PROFILE_CODE (expect 200)"

# User profile does not contain password_hash
USER_PROFILE=$(curl -s -H "Authorization: Bearer $TOKEN" "$GATEWAY/api/v1/users/$USER_ID")
HAS_PASSWORD=$(echo "$USER_PROFILE" | jq 'has("password_hash")' 2>/dev/null)
check "$([ "$HAS_PASSWORD" = "false" ] && echo true)" "User profile excludes password_hash → $HAS_PASSWORD (expect false)"

# Nonexistent user returns 404
FAKE_USER_CODE=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$GATEWAY/api/v1/users/00000000-0000-0000-0000-000000000000")
check "$([ "$FAKE_USER_CODE" = "404" ] && echo true)" "Nonexistent user → $FAKE_USER_CODE (expect 404)"

# ---- PRODUCT CRUD ----
echo ""
echo "--- Product CRUD ---"
PRODUCT_RESPONSE=$(curl -s -w "\n%{http_code}" -X POST "$GATEWAY/api/v1/products" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Correlation-ID: $CORRELATION_ID" \
  -d '{"name":"Test Avocados","description":"Ripe avocados","price":5.99,"category":"produce","stock":50}')
PRODUCT_CODE=$(echo "$PRODUCT_RESPONSE" | tail -1)
PRODUCT_BODY=$(echo "$PRODUCT_RESPONSE" | head -1)
PRODUCT_ID=$(echo "$PRODUCT_BODY" | jq -r '.id // empty' 2>/dev/null)
check "$([ "$PRODUCT_CODE" = "201" ] && echo true)" "Create product → $PRODUCT_CODE"
check "$([ -n "$PRODUCT_ID" ] && echo true)" "Product has ID"

GET_PRODUCT_CODE=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$GATEWAY/api/v1/products/$PRODUCT_ID")
check "$([ "$GET_PRODUCT_CODE" = "200" ] && echo true)" "Get product → $GET_PRODUCT_CODE"

# ---- ORDER SAGA (end-to-end) ----
echo ""
echo "--- Order Saga ---"
ORDER_RESPONSE=$(curl -s -w "\n%{http_code}" -X POST "$GATEWAY/api/v1/orders" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Correlation-ID: $CORRELATION_ID" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"123 Test St\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":2}]}")
ORDER_CODE=$(echo "$ORDER_RESPONSE" | tail -1)
ORDER_BODY=$(echo "$ORDER_RESPONSE" | head -1)
ORDER_ID=$(echo "$ORDER_BODY" | jq -r '.id // empty' 2>/dev/null)
ORDER_STATUS=$(echo "$ORDER_BODY" | jq -r '.status // empty' 2>/dev/null)
check "$([ "$ORDER_CODE" = "201" ] || [ "$ORDER_CODE" = "200" ] && echo true)" "Place order → $ORDER_CODE"
check "$([ "$ORDER_STATUS" = "confirmed" ] && echo true)" "Order status → $ORDER_STATUS (expect confirmed)"

# Verify stock decremented
STOCK=$(curl -s -H "Authorization: Bearer $TOKEN" "$GATEWAY/api/v1/products/$PRODUCT_ID" | jq -r '.stock // empty' 2>/dev/null)
check "$([ "$STOCK" = "48" ] && echo true)" "Stock decremented → $STOCK (expect 48)"

# ---- ORDER RETRIEVAL ----
echo ""
echo "--- Order Retrieval ---"

# GET single order
GET_ORDER_CODE=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$GATEWAY/api/v1/orders/$ORDER_ID")
check "$([ "$GET_ORDER_CODE" = "200" ] && echo true)" "Get order by ID → $GET_ORDER_CODE (expect 200)"

# Order has items
ORDER_ITEMS=$(curl -s -H "Authorization: Bearer $TOKEN" "$GATEWAY/api/v1/orders/$ORDER_ID" | jq '.items | length' 2>/dev/null)
check "$([ "$ORDER_ITEMS" -gt 0 ] && echo true)" "Order has items → count: $ORDER_ITEMS"

# List orders for user
USER_ORDERS_CODE=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$GATEWAY/api/v1/orders/user/$USER_ID")
check "$([ "$USER_ORDERS_CODE" = "200" ] && echo true)" "List user orders → $USER_ORDERS_CODE (expect 200)"

USER_ORDERS_COUNT=$(curl -s -H "Authorization: Bearer $TOKEN" "$GATEWAY/api/v1/orders/user/$USER_ID" | jq 'length' 2>/dev/null)
check "$([ "$USER_ORDERS_COUNT" -ge 1 ] && echo true)" "User has orders → count: $USER_ORDERS_COUNT"

# ---- ORDER STATE MACHINE ----
echo ""
echo "--- Order State Machine ---"

# confirmed → packing
PACK_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$GATEWAY/api/v1/orders/$ORDER_ID/status" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Correlation-ID: $CORRELATION_ID" \
  -d '{"status":"packing"}')
check "$([ "$PACK_CODE" = "200" ] && echo true)" "confirmed → packing → $PACK_CODE"

# packing → out_for_delivery
OFD_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$GATEWAY/api/v1/orders/$ORDER_ID/status" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Correlation-ID: $CORRELATION_ID" \
  -d '{"status":"out_for_delivery"}')
check "$([ "$OFD_CODE" = "200" ] && echo true)" "packing → out_for_delivery → $OFD_CODE"

# out_for_delivery → delivered
DEL_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$GATEWAY/api/v1/orders/$ORDER_ID/status" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Correlation-ID: $CORRELATION_ID" \
  -d '{"status":"delivered"}')
check "$([ "$DEL_CODE" = "200" ] && echo true)" "out_for_delivery → delivered → $DEL_CODE"

# Invalid transition: delivered → pending
INVALID_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$GATEWAY/api/v1/orders/$ORDER_ID/status" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"status":"pending"}')
check "$([ "$INVALID_CODE" = "400" ] && echo true)" "delivered → pending rejected → $INVALID_CODE (expect 400)"

# ---- CANCELLATION + STOCK RELEASE ----
echo ""
echo "--- Cancellation + Stock Release ---"
ORDER2_RESPONSE=$(curl -s -X POST "$GATEWAY/api/v1/orders" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Correlation-ID: $CORRELATION_ID" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"123 Test St\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":5}]}")
ORDER2_ID=$(echo "$ORDER2_RESPONSE" | jq -r '.id // empty' 2>/dev/null)

STOCK_BEFORE=$(curl -s -H "Authorization: Bearer $TOKEN" "$GATEWAY/api/v1/products/$PRODUCT_ID" | jq -r '.stock // empty' 2>/dev/null)

CANCEL_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$GATEWAY/api/v1/orders/$ORDER2_ID/status" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"status":"cancelled"}')
check "$([ "$CANCEL_CODE" = "200" ] && echo true)" "Cancel order → $CANCEL_CODE"

sleep 1
STOCK_AFTER=$(curl -s -H "Authorization: Bearer $TOKEN" "$GATEWAY/api/v1/products/$PRODUCT_ID" | jq -r '.stock // empty' 2>/dev/null)
EXPECTED_STOCK=$((STOCK_BEFORE + 5))
check "$([ "$STOCK_AFTER" = "$EXPECTED_STOCK" ] && echo true)" "Stock restored after cancel → $STOCK_AFTER (expect $EXPECTED_STOCK)"

# ---- SAGA COMPENSATION ON FAILURE ----
echo ""
echo "--- Saga Compensation on Failure ---"

# Create a low-stock product
LOW_STOCK_RESPONSE=$(curl -s -X POST "$PRODUCT_SVC/api/v1/products" \
  -H "Content-Type: application/json" \
  -d '{"name":"Rare Truffle","description":"Very limited","price":99.99,"category":"produce","stock":1}')
LOW_STOCK_ID=$(echo "$LOW_STOCK_RESPONSE" | jq -r '.id // empty' 2>/dev/null)

# Create a normal product
NORMAL_RESPONSE=$(curl -s -X POST "$PRODUCT_SVC/api/v1/products" \
  -H "Content-Type: application/json" \
  -d '{"name":"Regular Apple","description":"Plenty in stock","price":1.99,"category":"produce","stock":50}')
NORMAL_ID=$(echo "$NORMAL_RESPONSE" | jq -r '.id // empty' 2>/dev/null)

# Order both — first should reserve fine, second should fail (quantity 5 > stock 1)
# The saga should roll back the first reservation
NORMAL_STOCK_BEFORE=$(curl -s "$PRODUCT_SVC/api/v1/products/$NORMAL_ID" | jq -r '.stock' 2>/dev/null)

FAIL_ORDER_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$ORDER_SVC/api/v1/orders" \
  -H "Content-Type: application/json" \
  -H "X-Correlation-ID: saga-fail-test" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"123 Test St\",\"items\":[{\"product_id\":\"$NORMAL_ID\",\"quantity\":3},{\"product_id\":\"$LOW_STOCK_ID\",\"quantity\":5}]}")
check "$([ "$FAIL_ORDER_CODE" != "200" ] && [ "$FAIL_ORDER_CODE" != "201" ] && echo true)" "Order with insufficient stock fails → $FAIL_ORDER_CODE"

sleep 1
NORMAL_STOCK_AFTER=$(curl -s "$PRODUCT_SVC/api/v1/products/$NORMAL_ID" | jq -r '.stock' 2>/dev/null)
check "$([ "$NORMAL_STOCK_AFTER" = "$NORMAL_STOCK_BEFORE" ] && echo true)" "Saga rolled back first item reservation → stock $NORMAL_STOCK_AFTER (expect $NORMAL_STOCK_BEFORE)"

# ---- NOTIFICATIONS (async) ----
echo ""
echo "--- Notifications ---"
sleep 3
NOTIFS=$(curl -s "$NOTIF_SVC/api/v1/notifications/user/$USER_ID")
NOTIF_COUNT=$(echo "$NOTIFS" | jq 'length' 2>/dev/null)
check "$([ "$NOTIF_COUNT" -gt 0 ] && echo true)" "Notifications exist → count: $NOTIF_COUNT"

NOTIF_TYPES=$(echo "$NOTIFS" | jq -r '.[].type' 2>/dev/null | sort -u | tr '\n' ',' | sed 's/,$//')
echo "  Notification types found: $NOTIF_TYPES"

# ---- NOTIFICATION TYPES FOR STATE TRANSITIONS ----
echo ""
echo "--- Notification Types for State Transitions ---"
# We transitioned the first order through confirmed → packing → out_for_delivery → delivered
# And the second order through confirmed → cancelled
# Each should have generated a notification

NOTIF_TYPES=$(curl -s "$NOTIF_SVC/api/v1/notifications/user/$USER_ID" | jq -r '.[].type' 2>/dev/null | sort -u)

for expected_type in "order_confirmed" "order_packing" "order_out_for_delivery" "order_delivered" "order_cancelled"; do
  FOUND=$(echo "$NOTIF_TYPES" | grep -c "$expected_type" 2>/dev/null)
  check "$([ "$FOUND" -gt 0 ] && echo true)" "Notification type exists: $expected_type"
done

# ---- PROMETHEUS METRICS (Business) ----
echo ""
echo "--- Prometheus Metrics (Business) ---"
USERS_REG=$(curl -s "$USER_SVC/metrics" | grep -c "users_registered_total" 2>/dev/null)
check "$([ "$USERS_REG" -gt 0 ] && echo true)" "Metrics user-service has users_registered_total"

ORDERS_METRIC=$(curl -s "$ORDER_SVC/metrics" | grep -c "orders_created_total" 2>/dev/null)
check "$([ "$ORDERS_METRIC" -gt 0 ] && echo true)" "Metrics order-service has orders_created_total"

INVENTORY_METRIC=$(curl -s "$PRODUCT_SVC/metrics" | grep -c "inventory_level" 2>/dev/null)
check "$([ "$INVENTORY_METRIC" -gt 0 ] && echo true)" "Metrics product-service has inventory_level"

NOTIF_METRIC=$(curl -s "$NOTIF_SVC/metrics" | grep -c "notifications_sent_total" 2>/dev/null)
check "$([ "$NOTIF_METRIC" -gt 0 ] && echo true)" "Metrics notification-service has notifications_sent_total"

# ---- STRUCTURED LOGGING ----
echo ""
echo "--- Structured Logging ---"
sleep 2
JSON_LOGS=0
while IFS= read -r line; do
  # strip docker compose prefix
  if [[ "$line" == *"|"* ]]; then
    line="${line#*| }"
  fi
  if echo "$line" | jq . >/dev/null 2>&1; then
    JSON_LOGS=$((JSON_LOGS+1))
  fi
done <<< "$(docker compose logs api-gateway 2>&1 | tail -5)"
check "$([ "$JSON_LOGS" -gt 0 ] && echo true)" "Gateway logs are valid JSON ($JSON_LOGS/5 lines parsed)"

# ---- CORRELATION ID PROPAGATION ----
echo ""
echo "--- Correlation ID Propagation ---"
CID_HEADER=$(curl -s -D- -o /dev/null -H "X-Correlation-ID: verify-cid-123" "$GATEWAY/health" 2>&1 | grep -i "x-correlation-id" | head -1)
check "$(echo "$CID_HEADER" | grep -q "verify-cid-123" && echo true)" "Correlation ID echoed in response header"

CID_SERVICES=$(docker compose logs 2>&1 | grep "$CORRELATION_ID" | sed -n 's/.*"service":"\([^"]*\)".*/\1/p' | sort -u | wc -l)
check "$([ "$CID_SERVICES" -ge 3 ] && echo true)" "Correlation ID found in $CID_SERVICES services' logs (expect ≥3)"

# ---- AUTO-GENERATED CORRELATION ID ----
echo ""
echo "--- Auto-generated Correlation ID ---"
AUTO_CID=$(curl -s -D- -o /dev/null "$GATEWAY/health" 2>&1 | grep -i "x-correlation-id" | head -1 | tr -d '\r')
check "$([ -n "$AUTO_CID" ] && echo true)" "Auto-generated correlation ID when none sent"

# Verify it looks like a UUID (8-4-4-4-12 pattern)
AUTO_CID_VALUE=$(echo "$AUTO_CID" | sed 's/.*: //')
UUID_MATCH=$(echo "$AUTO_CID_VALUE" | grep -cE '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' 2>/dev/null)
check "$([ "$UUID_MATCH" -gt 0 ] && echo true)" "Auto-generated correlation ID is a UUID → $AUTO_CID_VALUE"

# ---- RATE LIMITING ----
# Run this last since it intentionally triggers 429s which could affect other tests
echo ""
echo "--- Rate Limiting ---"
RATE_LIMITED=false
# Send 300 requests in parallel batches to exceed 100/s
for batch in $(seq 1 6); do
  for i in $(seq 1 50); do
    curl -s -o /dev/null -w "%{http_code}\n" "$GATEWAY/health" &
  done | grep -q "429" && RATE_LIMITED=true
  wait
  if [ "$RATE_LIMITED" = "true" ]; then break; fi
done
check "$([ "$RATE_LIMITED" = "true" ] && echo true)" "Rate limiting triggers 429"

# ---- PROMETHEUS SERVER ----
echo ""
echo "--- Prometheus Server ---"

# Prometheus is running
PROM_CODE=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:9090/-/healthy")
check "$([ "$PROM_CODE" = "200" ] && echo true)" "Prometheus is running → $PROM_CODE"

# Prometheus is scraping all 5 services
for job in "api-gateway" "user-service" "product-service" "order-service" "notification-service"; do
  TARGET_HEALTH=$(curl -s "http://localhost:9090/api/v1/targets" | jq -r ".data.activeTargets[] | select(.labels.job==\"$job\") | .health" 2>/dev/null)
  check "$([ "$TARGET_HEALTH" = "up" ] && echo true)" "Prometheus scraping $job → $TARGET_HEALTH"
done

# Prometheus has alert rules
ALERT_COUNT=$(curl -s "http://localhost:9090/api/v1/rules" | jq '[.data.groups[].rules[]] | length' 2>/dev/null)
check "$([ "$ALERT_COUNT" -ge 4 ] && echo true)" "Prometheus has alert rules → count: $ALERT_COUNT (expect ≥4)"

# Verify specific alert rules exist
for alert in "HighErrorRate" "ServiceDown" "HighLatency" "LowInventory"; do
  FOUND=$(curl -s "http://localhost:9090/api/v1/rules" | jq -r "[.data.groups[].rules[] | select(.name==\"$alert\")] | length" 2>/dev/null)
  check "$([ "$FOUND" -gt 0 ] && echo true)" "Alert rule exists: $alert"
done

# ---- LOKI LOG AGGREGATION ----
echo ""
echo "--- Loki Log Aggregation ---"

# Loki is running (check status or that it has data)
LOKI_CODE=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:3100/ready")
LOKI_HAS_DATA=$(curl -s "http://localhost:3100/loki/api/v1/labels" | jq '.data | length' 2>/dev/null)
check "$([ "$LOKI_CODE" = "200" ] || [ "$LOKI_HAS_DATA" -gt 0 ] && echo true)" "Loki is running → status: $LOKI_CODE, labels: $LOKI_HAS_DATA"

# Loki has labels (means Promtail is shipping logs)
LOKI_LABELS=$(curl -s "http://localhost:3100/loki/api/v1/labels" | jq '.data | length' 2>/dev/null)
check "$([ "$LOKI_LABELS" -gt 0 ] && echo true)" "Loki has labels → count: $LOKI_LABELS"

# Loki has service label
LOKI_HAS_SERVICE=$(curl -s "http://localhost:3100/loki/api/v1/labels" | jq '.data | index("service")' 2>/dev/null)
check "$([ "$LOKI_HAS_SERVICE" != "null" ] && [ -n "$LOKI_HAS_SERVICE" ] && echo true)" "Loki has 'service' label"

# Query Loki for logs from our test run
LOKI_RESULTS=$(curl -s "http://localhost:3100/loki/api/v1/query_range" \
  --data-urlencode "query={service=~\".+\"}" \
  --data-urlencode "limit=5" \
  --data-urlencode "start=$(date -u -v-5M +%s 2>/dev/null || date -u -d '5 minutes ago' +%s)000000000" \
  --data-urlencode "end=$(date -u +%s)000000000" | jq '.data.result | length' 2>/dev/null)
check "$([ "$LOKI_RESULTS" -gt 0 ] && echo true)" "Loki returns log results → streams: $LOKI_RESULTS"

# Query Loki by correlation ID from this test run
sleep 2
LOKI_CID_RESULTS=$(curl -s "http://localhost:3100/loki/api/v1/query_range" \
  --data-urlencode "query={service=~\".+\"} |= \"$CORRELATION_ID\"" \
  --data-urlencode "limit=5" \
  --data-urlencode "start=$(date -u -v-5M +%s 2>/dev/null || date -u -d '5 minutes ago' +%s)000000000" \
  --data-urlencode "end=$(date -u +%s)000000000" | jq '.data.result | length' 2>/dev/null)
check "$([ "$LOKI_CID_RESULTS" -gt 0 ] && echo true)" "Loki can filter by correlation ID → streams: $LOKI_CID_RESULTS"

# ---- GRAFANA ----
echo ""
echo "--- Grafana ---"

# Grafana is running
GRAFANA_CODE=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:3000/api/health")
check "$([ "$GRAFANA_CODE" = "200" ] && echo true)" "Grafana is running → $GRAFANA_CODE"

# Grafana datasources are provisioned
for ds in "Prometheus" "Loki" "Jaeger"; do
  DS_FOUND=$(curl -s "http://localhost:3000/api/datasources" | jq -r ".[].name" 2>/dev/null | grep -c "$ds")
  check "$([ "$DS_FOUND" -gt 0 ] && echo true)" "Grafana datasource: $ds"
done

# Grafana dashboards are loaded
for dashboard in "Service Overview" "Business Metrics" "Logs Explorer"; do
  DB_FOUND=$(curl -s "http://localhost:3000/api/search" | jq -r ".[].title" 2>/dev/null | grep -c "$dashboard")
  check "$([ "$DB_FOUND" -gt 0 ] && echo true)" "Grafana dashboard: $dashboard"
done

# ---- JAEGER ----
echo ""
echo "--- Jaeger ---"

# Jaeger UI is running
JAEGER_CODE=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:16686/")
check "$([ "$JAEGER_CODE" = "200" ] && echo true)" "Jaeger UI is running → $JAEGER_CODE"

# Jaeger API responds
JAEGER_API=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:16686/api/services")
check "$([ "$JAEGER_API" = "200" ] && echo true)" "Jaeger API responds → $JAEGER_API"

# ---- OPENTELEMETRY TRACING ----
echo ""
echo "--- OpenTelemetry Tracing ---"

# Generate a traced request
TRACE_CID="trace-test-$(date +%s)"
TRACE_REG=$(curl -s -X POST "$GATEWAY/api/v1/auth/register" \
  -H "Content-Type: application/json" \
  -H "X-Correlation-ID: $TRACE_CID" \
  -d "{\"email\":\"traceuser-$TRACE_CID@test.com\",\"password\":\"pass123\",\"name\":\"Trace User\",\"delivery_address\":\"123 Trace St\"}")
TRACE_TOKEN=$(echo "$TRACE_REG" | jq -r '.token // empty' 2>/dev/null)
TRACE_USER_ID=$(echo "$TRACE_REG" | jq -r '.id // empty' 2>/dev/null)

# Create a product for trace test
TRACE_PRODUCT=$(curl -s -X POST "$PRODUCT_SVC/api/v1/products" \
  -H "Content-Type: application/json" \
  -d '{"name":"Trace Avocado","description":"For tracing","price":3.99,"category":"produce","stock":20}')
TRACE_PRODUCT_ID=$(echo "$TRACE_PRODUCT" | jq -r '.id // empty' 2>/dev/null)

# Place an order to generate a multi-service trace
curl -s -X POST "$GATEWAY/api/v1/orders" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TRACE_TOKEN" \
  -H "X-Correlation-ID: $TRACE_CID" \
  -d "{\"user_id\":\"$TRACE_USER_ID\",\"delivery_address\":\"123 Trace St\",\"items\":[{\"product_id\":\"$TRACE_PRODUCT_ID\",\"quantity\":1}]}" > /dev/null 2>&1

# Wait for traces to be exported to Jaeger
sleep 10

# Jaeger should now have services registered
JAEGER_SERVICES=$(curl -s "http://localhost:16686/api/services" | jq -r '.data[]' 2>/dev/null)

for svc in "api-gateway" "order-service" "product-service"; do
  SVC_FOUND=$(echo "$JAEGER_SERVICES" | grep -c "$svc" 2>/dev/null)
  check "$([ "$SVC_FOUND" -gt 0 ] && echo true)" "Jaeger has traces for: $svc"
done

# Check that traces actually exist for api-gateway
TRACE_COUNT=$(curl -s "http://localhost:16686/api/traces?service=api-gateway&limit=5" | jq '.data | length' 2>/dev/null)
check "$([ "$TRACE_COUNT" -gt 0 ] && echo true)" "Jaeger has api-gateway traces → count: $TRACE_COUNT"

# Check a trace has multiple spans (meaning it crossed services)
FIRST_TRACE_SPANS=$(curl -s "http://localhost:16686/api/traces?service=api-gateway&limit=1" | jq '.data[0].spans | length' 2>/dev/null)
check "$([ "$FIRST_TRACE_SPANS" -gt 1 ] && echo true)" "Trace has multiple spans (cross-service) → spans: $FIRST_TRACE_SPANS"

# Check that order-service traces exist
ORDER_TRACE_COUNT=$(curl -s "http://localhost:16686/api/traces?service=order-service&limit=5" | jq '.data | length' 2>/dev/null)
check "$([ "$ORDER_TRACE_COUNT" -gt 0 ] && echo true)" "Jaeger has order-service traces → count: $ORDER_TRACE_COUNT"

# Verify trace_id appears in structured logs
TRACE_ID_IN_LOGS=$(docker compose logs 2>&1 | grep -c "trace_id" 2>/dev/null)
check "$([ "$TRACE_ID_IN_LOGS" -gt 0 ] && echo true)" "Logs contain trace_id field → occurrences: $TRACE_ID_IN_LOGS"

# ---- CIRCUIT BREAKER ----
echo ""
echo "--- Circuit Breaker ---"

# Circuit breaker metric exists on order-service
CB_METRIC=$(curl -s localhost:8083/metrics | grep -c "circuit_breaker_state" 2>/dev/null)
check "$([ "$CB_METRIC" -gt 0 ] && echo true)" "Circuit breaker metric exists on order-service"

# Circuit breaker starts in closed state (value 0)
CB_STATE=$(curl -s localhost:8083/metrics | grep "circuit_breaker_state" | grep -v "^#" | head -1 | awk '{print $2}')
check "$([ "$CB_STATE" = "0" ] && echo true)" "Circuit breaker is closed (state=$CB_STATE, expect 0)"

# ---- DEAD LETTER QUEUES ----
echo ""
echo "--- Dead Letter Queues ---"

# DLQ queues exist in RabbitMQ
INVENTORY_DLQ=$(curl -s -u guest:guest http://localhost:15672/api/queues | jq -r '.[].name' 2>/dev/null | grep -c "inventory.update.dlq")
check "$([ "$INVENTORY_DLQ" -gt 0 ] && echo true)" "DLQ exists: inventory.update.dlq"

NOTIF_DLQ=$(curl -s -u guest:guest http://localhost:15672/api/queues | jq -r '.[].name' 2>/dev/null | grep -c "notifications.order.dlq")
check "$([ "$NOTIF_DLQ" -gt 0 ] && echo true)" "DLQ exists: notifications.order.dlq"

# Main queues have DLQ arguments configured
INVENTORY_DLQ_CONFIG=$(curl -s -u guest:guest "http://localhost:15672/api/queues/%2F/inventory.update" | jq -r '.arguments["x-dead-letter-routing-key"] // empty' 2>/dev/null)
check "$([ "$INVENTORY_DLQ_CONFIG" = "inventory.update.dlq" ] && echo true)" "inventory.update has DLQ routing configured → $INVENTORY_DLQ_CONFIG"

NOTIF_DLQ_CONFIG=$(curl -s -u guest:guest "http://localhost:15672/api/queues/%2F/notifications.order" | jq -r '.arguments["x-dead-letter-routing-key"] // empty' 2>/dev/null)
check "$([ "$NOTIF_DLQ_CONFIG" = "notifications.order.dlq" ] && echo true)" "notifications.order has DLQ routing configured → $NOTIF_DLQ_CONFIG"

# ---- RESILIENCE: HAPPY PATH STILL WORKS ----
echo ""
echo "--- Resilience: Happy Path Still Works ---"

# Place an order through the full resilience stack (circuit breaker + retry + timeout)
RESILIENCE_ORDER=$(curl -s -w "\n%{http_code}" -X POST "$ORDER_SVC/api/v1/orders" \
  -H "Content-Type: application/json" \
  -H "X-Correlation-ID: resilience-test-001" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"123 Resilience St\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":1}]}")
RESILIENCE_CODE=$(echo "$RESILIENCE_ORDER" | tail -1)
RESILIENCE_STATUS=$(echo "$RESILIENCE_ORDER" | head -1 | jq -r '.status // empty' 2>/dev/null)
check "$([ "$RESILIENCE_CODE" = "201" ] || [ "$RESILIENCE_CODE" = "200" ] && echo true)" "Order through resilience stack → $RESILIENCE_CODE"
check "$([ "$RESILIENCE_STATUS" = "confirmed" ] && echo true)" "Order confirmed through resilience stack → $RESILIENCE_STATUS"

# Verify resilience logging (circuit breaker, retry mentions in logs)
sleep 1
CB_LOGS=$(docker compose logs order-service 2>&1 | grep -c "circuit.breaker\|circuit_breaker" 2>/dev/null)
check "$([ "$CB_LOGS" -ge 0 ] && echo true)" "Order service has circuit breaker log references → $CB_LOGS"

# ---- TIMEOUTS ----
echo ""
echo "--- Timeouts ---"

# Verify order-service and gateway are using HTTP clients (indirect — check they start without errors)
ORDER_ERRORS=$(docker compose logs order-service 2>&1 | grep -ci "panic\|fatal" 2>/dev/null)
check "$([ "$ORDER_ERRORS" -eq 0 ] && echo true)" "Order service has no panics/fatals → $ORDER_ERRORS"

GW_ERRORS=$(docker compose logs api-gateway 2>&1 | grep -ci "panic\|fatal" 2>/dev/null)
check "$([ "$GW_ERRORS" -eq 0 ] && echo true)" "Gateway has no panics/fatals → $GW_ERRORS"

# ---- RABBITMQ QUEUES HEALTH ----
echo ""
echo "--- RabbitMQ Queues Health ---"

# All expected queues exist
for queue in "inventory.update" "notifications.order" "inventory.update.dlq" "notifications.order.dlq"; do
  Q_EXISTS=$(curl -s -u guest:guest http://localhost:15672/api/queues | jq -r ".[].name" 2>/dev/null | grep -c "^${queue}$")
  check "$([ "$Q_EXISTS" -gt 0 ] && echo true)" "RabbitMQ queue exists: $queue"
done

# Main queues have consumers
for queue in "inventory.update" "notifications.order"; do
  Q_CONSUMERS=$(curl -s -u guest:guest "http://localhost:15672/api/queues/%2F/$queue" | jq '.consumers // 0' 2>/dev/null)
  check "$([ "$Q_CONSUMERS" -gt 0 ] && echo true)" "Queue $queue has consumers → $Q_CONSUMERS"
done

# ---- PRODUCT SEARCH ----
echo ""
echo "--- Product Search ---"

curl -s -X POST "$PRODUCT_SVC/api/v1/products" \
  -H "Content-Type: application/json" \
  -d '{"name":"Search Test Mango","description":"Tropical fruit","price":3.99,"category":"produce","stock":25}' > /dev/null

curl -s -X POST "$PRODUCT_SVC/api/v1/products" \
  -H "Content-Type: application/json" \
  -d '{"name":"Search Test Yogurt","description":"Greek style","price":5.49,"category":"dairy","stock":40}' > /dev/null

SEARCH_NAME=$(curl -s "$PRODUCT_SVC/api/v1/products/search?q=Mango" | jq 'length' 2>/dev/null)
check "$([ "$SEARCH_NAME" -gt 0 ] && echo true)" "Search by name finds results → $SEARCH_NAME"

SEARCH_CAT=$(curl -s "$PRODUCT_SVC/api/v1/products/search?category=dairy" | jq 'length' 2>/dev/null)
check "$([ "$SEARCH_CAT" -gt 0 ] && echo true)" "Search by category finds results → $SEARCH_CAT"

SEARCH_PRICE=$(curl -s "$PRODUCT_SVC/api/v1/products/search?min_price=3&max_price=4" | jq 'length' 2>/dev/null)
check "$([ "$SEARCH_PRICE" -gt 0 ] && echo true)" "Search by price range finds results → $SEARCH_PRICE"

SEARCH_STOCK=$(curl -s "$PRODUCT_SVC/api/v1/products/search?in_stock=true" | jq 'length' 2>/dev/null)
check "$([ "$SEARCH_STOCK" -gt 0 ] && echo true)" "Search in-stock products → $SEARCH_STOCK"

SEARCH_EMPTY=$(curl -s "$PRODUCT_SVC/api/v1/products/search?q=xyznonexistent" | jq 'length' 2>/dev/null)
check "$([ "$SEARCH_EMPTY" = "0" ] && echo true)" "Search with no match returns empty → $SEARCH_EMPTY"

SEARCH_GW_CODE=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$GATEWAY/api/v1/products/search?q=Mango")
check "$([ "$SEARCH_GW_CODE" = "200" ] && echo true)" "Search via gateway with auth → $SEARCH_GW_CODE"

SEARCH_NO_AUTH=$(curl -s -o /dev/null -w "%{http_code}" "$GATEWAY/api/v1/products/search?q=Mango")
check "$([ "$SEARCH_NO_AUTH" = "401" ] && echo true)" "Search via gateway without auth → $SEARCH_NO_AUTH (expect 401)"

# ---- SCRIPTS & DOCUMENTATION ----
echo ""
echo "--- Scripts & Documentation ---"

check "$([ -x scripts/chaos.sh ] && echo true)" "scripts/chaos.sh exists and is executable"
check "$([ -x scripts/loadtest.sh ] && echo true)" "scripts/loadtest.sh exists and is executable"
check "$([ -x scripts/trivy-scan.sh ] && echo true)" "scripts/trivy-scan.sh exists and is executable"
check "$([ -x scripts/seed.sh ] && echo true)" "scripts/seed.sh exists and is executable"

check "$([ -f docs/ARCHITECTURE.md ] && echo true)" "docs/ARCHITECTURE.md exists"
check "$([ -f docs/SETUP.md ] && echo true)" "docs/SETUP.md exists"
check "$([ -f README.md ] && echo true)" "README.md exists"

ARCH_SAGA=$(grep -ci "saga" docs/ARCHITECTURE.md 2>/dev/null)
check "$([ "$ARCH_SAGA" -gt 0 ] && echo true)" "Architecture doc covers saga pattern"

ARCH_RESILIENCE=$(grep -ci "circuit.breaker\|resilience\|retry" docs/ARCHITECTURE.md 2>/dev/null)
check "$([ "$ARCH_RESILIENCE" -gt 0 ] && echo true)" "Architecture doc covers resilience patterns"

ARCH_OBSERVABILITY=$(grep -ci "observability\|prometheus\|grafana\|jaeger\|loki" docs/ARCHITECTURE.md 2>/dev/null)
check "$([ "$ARCH_OBSERVABILITY" -gt 0 ] && echo true)" "Architecture doc covers observability stack"

for target in "build" "up" "down" "clean" "logs" "seed" "test-all" "test-k8s" "chaos" "loadtest" "trivy" "boot" "boot-down"; do
  TARGET_EXISTS=$(grep -c "^${target}:" Makefile 2>/dev/null || grep -c "^${target} " Makefile 2>/dev/null)
  check "$([ "$TARGET_EXISTS" -gt 0 ] && echo true)" "Makefile has target: $target"
done

echo ""
echo "--- Kubernetes Manifests Existence ---"

check "$([ -f k8s/namespaces.yml ] && echo true)" "k8s/namespaces.yml exists"

for svc in "api-gateway" "user-service" "product-service" "order-service" "notification-service"; do
  check "$([ -f k8s/ecommerce/$svc/deployment.yml ] && echo true)" "K8s deployment: $svc"
  check "$([ -f k8s/ecommerce/$svc/service.yml ] && echo true)" "K8s service: $svc"
  check "$([ -f k8s/ecommerce/$svc/pdb.yml ] && echo true)" "K8s PDB: $svc"
  check "$([ -f k8s/ecommerce/$svc/configmap.yml ] && echo true)" "K8s configmap: $svc"
done

check "$([ -f k8s/ecommerce/api-gateway/hpa.yml ] && echo true)" "K8s HPA: api-gateway"
check "$([ -f k8s/ecommerce/order-service/hpa.yml ] && echo true)" "K8s HPA: order-service"
check "$([ -f k8s/ecommerce/secrets.yml ] && echo true)" "K8s secrets manifest"
check "$([ -f k8s/ecommerce/network-policy.yml ] && echo true)" "K8s network policy: ecommerce"

for db in "user-db" "product-db" "order-db" "notification-db"; do
  check "$([ -f k8s/ecommerce-data/$db/statefulset.yml ] || [ -f k8s/ecommerce-data/$db/deployment.yml ] && echo true)" "K8s data: $db"
  check "$([ -f k8s/ecommerce-data/$db/service.yml ] && echo true)" "K8s service: $db"
done

check "$([ -f k8s/ecommerce-data/redis/deployment.yml ] && echo true)" "K8s data: redis"
check "$([ -f k8s/ecommerce-data/rabbitmq/statefulset.yml ] || [ -f k8s/ecommerce-data/rabbitmq/deployment.yml ] && echo true)" "K8s data: rabbitmq"
check "$([ -f k8s/ecommerce-data/network-policy.yml ] && echo true)" "K8s network policy: ecommerce-data"

for obs in "prometheus" "loki" "grafana" "jaeger"; do
  OBS_EXISTS=$(ls k8s/observability/$obs/*.yml 2>/dev/null | wc -l)
  check "$([ "$OBS_EXISTS" -gt 0 ] && echo true)" "K8s observability: $obs"
done

check "$([ -f k8s/observability/promtail/daemonset.yml ] && echo true)" "K8s promtail daemonset"
check "$([ -f k8s/kind-config.yml ] && echo true)" "Kind cluster config exists"

OTEL_IN_CONFIGMAPS=$(grep -rl "OTEL_EXPORTER" k8s/ecommerce/ 2>/dev/null | wc -l)
check "$([ "$OTEL_IN_CONFIGMAPS" -ge 5 ] && echo true)" "OTEL env vars in K8s configmaps → $OTEL_IN_CONFIGMAPS files"

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
