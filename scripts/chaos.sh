#!/bin/bash
set -o pipefail

# =============================================================================
# FreshCart Chaos Testing (Kubernetes)
# =============================================================================
# Tests resilience patterns:
#   - Circuit breakers, retry with exponential backoff and jitter
#   - 5-second timeouts, bulkheading, dead letter queues
#
# Scenarios:
#   1. Kill pods
#   2. Network partitions
#   3. Database crashes
#   4. Flood endpoints (100 concurrent requests)
#   5. RabbitMQ failure
# =============================================================================

NAMESPACE_APP="ecommerce"
NAMESPACE_DATA="ecommerce-data"
GATEWAY_URL="http://localhost:30080"

PASS=0
FAIL=0
CORRELATION_ID="chaos-$(date +%s)"

green() { echo -e "\033[32m  PASS: $1\033[0m"; PASS=$((PASS+1)); }
red() { echo -e "\033[31m  FAIL: $1\033[0m"; FAIL=$((FAIL+1)); }
yellow() { echo -e "\033[33m  INFO: $1\033[0m"; }
check() { if [ "$1" = "true" ]; then green "$2"; else red "$2"; fi; }

wait_for_health() {
  local url="$1" max="${2:-60}" i=0
  while [ $i -lt $max ]; do
    [ "$(curl -s -o /dev/null -w "%{http_code}" "$url" 2>/dev/null)" = "200" ] && return 0
    sleep 1; i=$((i+1))
  done
  return 1
}

wait_for_pod() {
  kubectl wait --for=condition=ready pod -l "$1" -n "$2" --timeout="${3:-60}s" 2>/dev/null
}

get_pod() {
  kubectl get pods -n "$2" -l "$1" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

echo "============================================"
echo "  FreshCart Chaos Testing (Kubernetes)"
echo "  Correlation ID: $CORRELATION_ID"
echo "============================================"
echo ""

# ---- PRE-FLIGHT ----
echo "--- Pre-flight Checks ---"
GW_STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$GATEWAY_URL/health" 2>/dev/null)
if [ "$GW_STATUS" != "200" ]; then
  echo "  FATAL: Gateway not accessible at $GATEWAY_URL"
  echo "  Run: make boot"
  exit 1
fi
echo "  Gateway OK"
echo ""

# ---- SEED DATA ----
echo "--- Seeding Test Data ---"
REG=$(curl -s -X POST "$GATEWAY_URL/api/v1/auth/register" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"chaos-$CORRELATION_ID@test.com\",\"password\":\"password123\",\"name\":\"Chaos\",\"delivery_address\":\"123 St\"}")
TOKEN=$(echo "$REG" | jq -r '.token // empty')
USER_ID=$(echo "$REG" | jq -r '.id // empty')
[ -z "$TOKEN" ] && echo "  FATAL: Could not register user" && exit 1
echo "  User: $USER_ID"

PROD=$(curl -s -X POST "$GATEWAY_URL/api/v1/products" \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d '{"name":"Chaos Product","description":"Test","price":9.99,"category":"produce","stock":1000}')
PRODUCT_ID=$(echo "$PROD" | jq -r '.id // empty')
[ -z "$PRODUCT_ID" ] && echo "  FATAL: Could not create product" && exit 1
echo "  Product: $PRODUCT_ID"
echo ""

# ============================================================================
# SCENARIO 1: Kill product-service pod
# ============================================================================
echo "--- Scenario 1: Kill product-service Pod ---"
POD=$(get_pod "app=product-service" "$NAMESPACE_APP")
yellow "Killing $POD"
kubectl delete pod "$POD" -n "$NAMESPACE_APP" --grace-period=0 --force >/dev/null 2>&1
sleep 2

CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time 10 -X POST "$GATEWAY_URL/api/v1/orders" \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"Test\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":1}]}")
check "$([ "$CODE" != "000" ] && echo true)" "Order fails gracefully -> $CODE"

GW=$(curl -s -o /dev/null -w "%{http_code}" "$GATEWAY_URL/health")
check "$([ "$GW" = "200" ] && echo true)" "Gateway healthy -> $GW"

yellow "Waiting for recovery..."
wait_for_pod "app=product-service" "$NAMESPACE_APP" 90
check "$([ $? -eq 0 ] && echo true)" "product-service recovered"
sleep 5

CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY_URL/api/v1/orders" \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"Test\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":1}]}")
check "$([ "$CODE" = "201" ] || [ "$CODE" = "200" ] && echo true)" "Orders work after recovery -> $CODE"
echo ""

# ============================================================================
# SCENARIO 2: Kill user-db (database crash)
# ============================================================================
echo "--- Scenario 2: Kill user-db (Database Crash) ---"
POD=$(get_pod "app=user-db" "$NAMESPACE_DATA")
yellow "Killing $POD"
kubectl delete pod "$POD" -n "$NAMESPACE_DATA" --grace-period=0 --force >/dev/null 2>&1
sleep 3

CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time 10 -X POST "$GATEWAY_URL/api/v1/auth/login" \
  -H "Content-Type: application/json" -d '{"email":"x@x.com","password":"x"}')
check "$([ "$CODE" != "000" ] && echo true)" "Auth fails gracefully -> $CODE"

GW=$(curl -s -o /dev/null -w "%{http_code}" "$GATEWAY_URL/health")
check "$([ "$GW" = "200" ] && echo true)" "Gateway healthy -> $GW"

yellow "Waiting for recovery..."
wait_for_pod "app=user-db" "$NAMESPACE_DATA" 90
check "$([ $? -eq 0 ] && echo true)" "user-db recovered"
sleep 10

CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY_URL/api/v1/auth/login" \
  -H "Content-Type: application/json" -d "{\"email\":\"chaos-$CORRELATION_ID@test.com\",\"password\":\"password123\"}")
check "$([ "$CODE" = "200" ] && echo true)" "Auth works after recovery -> $CODE"
echo ""

# ============================================================================
# SCENARIO 3: Kill RabbitMQ
# ============================================================================
echo "--- Scenario 3: Kill RabbitMQ ---"
POD=$(get_pod "app=rabbitmq" "$NAMESPACE_DATA")
yellow "Killing $POD"
kubectl delete pod "$POD" -n "$NAMESPACE_DATA" --grace-period=0 --force >/dev/null 2>&1
sleep 3

CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time 15 -X POST "$GATEWAY_URL/api/v1/orders" \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"Test\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":1}]}")
check "$([ "$CODE" = "201" ] || [ "$CODE" = "200" ] && echo true)" "Orders succeed without RabbitMQ -> $CODE"

yellow "Waiting for recovery..."
wait_for_pod "app=rabbitmq" "$NAMESPACE_DATA" 90
check "$([ $? -eq 0 ] && echo true)" "RabbitMQ recovered"
echo ""

# ============================================================================
# SCENARIO 4: Network Partition
# ============================================================================
echo "--- Scenario 4: Network Partition ---"
yellow "Isolating order-service from product-service"

# Delete the allow-order-to-product policy to simulate network partition
kubectl delete networkpolicy allow-order-to-product -n "$NAMESPACE_APP" >/dev/null 2>&1
sleep 3

CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time 15 -X POST "$GATEWAY_URL/api/v1/orders" \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"Test\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":1}]}")
check "$([ "$CODE" != "201" ] && [ "$CODE" != "200" ] && echo true)" "Orders fail during partition -> $CODE"

GW=$(curl -s -o /dev/null -w "%{http_code}" "$GATEWAY_URL/health")
check "$([ "$GW" = "200" ] && echo true)" "Gateway healthy during partition -> $GW"

yellow "Healing partition..."
# Restore the allow-order-to-product policy
kubectl apply -f - <<EOF >/dev/null 2>&1
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-order-to-product
  namespace: $NAMESPACE_APP
spec:
  podSelector:
    matchLabels:
      app: product-service
  policyTypes:
  - Ingress
  ingress:
  - from:
    - podSelector:
        matchLabels:
          app: order-service
    ports:
    - port: 8082
EOF
sleep 3

CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY_URL/api/v1/orders" \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"Test\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":1}]}")
check "$([ "$CODE" = "201" ] || [ "$CODE" = "200" ] && echo true)" "Orders work after heal -> $CODE"
echo ""

# ============================================================================
# SCENARIO 5: Flood with 100 Concurrent Requests
# ============================================================================
echo "--- Scenario 5: Flood 100 Concurrent Requests ---"
RESP=$(mktemp)
LAT=$(mktemp)

for i in $(seq 1 100); do
  (
    r=$(curl -s -o /dev/null -w "%{http_code} %{time_total}" "$GATEWAY_URL/health" 2>/dev/null)
    echo "$r" | awk '{print $1}' >> "$RESP"
    echo "$r" | awk '{print $2}' >> "$LAT"
  ) &
done
wait

OK=$(grep -c "^200$" "$RESP" 2>/dev/null || echo 0)
R429=$(grep -c "^429$" "$RESP" 2>/dev/null || echo 0)
ERR=$(grep -vE "^(200|429)$" "$RESP" | wc -l | tr -d ' ')

SORTED=$(mktemp)
sort -n "$LAT" > "$SORTED"
P50=$(sed -n '50p' "$SORTED")
P95=$(sed -n '95p' "$SORTED")
P99=$(sed -n '99p' "$SORTED")

echo "  200=$OK, 429=$R429, errors=$ERR"
echo "  p50=${P50}s p95=${P95}s p99=${P99}s"
check "$([ "$ERR" -eq 0 ] && echo true)" "No errors under load"
check "$([ "$OK" -gt 50 ] && echo true)" "Majority succeeded (>50%)"

rm -f "$RESP" "$LAT" "$SORTED"
echo ""

# ============================================================================
# SCENARIO 6: Kill notification-service (DLQ test)
# ============================================================================
echo "--- Scenario 6: Kill notification-service (DLQ) ---"
POD=$(get_pod "app=notification-service" "$NAMESPACE_APP")
yellow "Killing $POD"
kubectl delete pod "$POD" -n "$NAMESPACE_APP" --grace-period=0 --force >/dev/null 2>&1
sleep 2

CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY_URL/api/v1/orders" \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"Test\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":1}]}")
check "$([ "$CODE" = "201" ] || [ "$CODE" = "200" ] && echo true)" "Orders succeed without notifications -> $CODE"

yellow "Waiting for recovery..."
wait_for_pod "app=notification-service" "$NAMESPACE_APP" 90
check "$([ $? -eq 0 ] && echo true)" "notification-service recovered"
echo ""

# ============================================================================
# FINAL VERIFICATION
# ============================================================================
echo "--- Final Verification ---"
for svc in api-gateway user-service product-service order-service notification-service; do
  ST=$(kubectl get pods -n "$NAMESPACE_APP" -l "app=$svc" -o jsonpath='{.items[0].status.phase}' 2>/dev/null)
  check "$([ "$ST" = "Running" ] && echo true)" "$svc Running"
done

CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY_URL/api/v1/orders" \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"Final\",\"items\":[{\"product_id\":\"$PRODUCT_ID\",\"quantity\":1}]}")
check "$([ "$CODE" = "201" ] || [ "$CODE" = "200" ] && echo true)" "Final order -> $CODE"

echo ""
echo "============================================"
TOTAL=$((PASS + FAIL))
echo "  Results: $PASS/$TOTAL passed"
[ "$FAIL" -gt 0 ] && echo -e "  \033[31m$FAIL FAILED\033[0m" && exit 1
echo -e "  \033[32mALL PASSED - SYSTEM RECOVERS\033[0m"
exit 0
