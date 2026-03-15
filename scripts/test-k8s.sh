#!/bin/bash
set -o pipefail

GATEWAY="${GATEWAY_URL:-http://localhost:30080}"
GRAFANA="${GRAFANA_URL:-http://localhost:30030}"
JAEGER="${JAEGER_URL:-http://localhost:30086}"

PASS=0
FAIL=0
K8S_EMAIL="k8stest-$(date +%s)@test.com"

green() { echo -e "\033[32m  PASS: $1\033[0m"; PASS=$((PASS+1)); }
red() { echo -e "\033[31m  FAIL: $1\033[0m"; FAIL=$((FAIL+1)); }
check() {
  if [ "$1" = "true" ]; then green "$2"; else red "$2"; fi
}

echo "============================================"
echo "  FreshCart Kubernetes Full Test Suite"
echo "  Gateway: $GATEWAY"
echo "  Grafana: $GRAFANA"
echo "  Jaeger:  $JAEGER"
echo "============================================"
echo ""

# ---- NAMESPACES ----
echo "--- Namespaces ---"
for ns in "ecommerce" "ecommerce-data" "observability"; do
  NS_EXISTS=$(kubectl get namespace "$ns" --no-headers 2>/dev/null | grep -c "$ns")
  check "$([ "$NS_EXISTS" -gt 0 ] && echo true)" "Namespace exists: $ns"
done

# ---- ECOMMERCE PODS ----
echo ""
echo "--- Ecommerce Pods (>=2 Running) ---"
for app in "api-gateway" "user-service" "product-service" "order-service" "notification-service"; do
  RUNNING=$(kubectl get pods -n ecommerce -l app="$app" --no-headers 2>/dev/null | grep -c "Running")
  check "$([ "$RUNNING" -ge 2 ] && echo true)" "$app has >=2 Running pods -> $RUNNING"
done

# ---- DATA PODS ----
echo ""
echo "--- Data Pods (>=1 Running) ---"
for app in "user-db" "product-db" "order-db" "notification-db" "redis" "rabbitmq"; do
  RUNNING=$(kubectl get pods -n ecommerce-data -l app="$app" --no-headers 2>/dev/null | grep -c "Running")
  check "$([ "$RUNNING" -ge 1 ] && echo true)" "$app is running -> $RUNNING pods"
done

# ---- OBSERVABILITY PODS ----
echo ""
echo "--- Observability Pods (>=1 Running) ---"
for app in "prometheus" "loki" "grafana" "jaeger"; do
  RUNNING=$(kubectl get pods -n observability -l app="$app" --no-headers 2>/dev/null | grep -c "Running")
  check "$([ "$RUNNING" -ge 1 ] && echo true)" "$app is running -> $RUNNING pods"
done

PROMTAIL_RUNNING=$(kubectl get pods -n observability -l app=promtail --no-headers 2>/dev/null | grep -c "Running")
check "$([ "$PROMTAIL_RUNNING" -ge 1 ] && echo true)" "promtail daemonset running -> $PROMTAIL_RUNNING pods"

# ---- HPA ----
echo ""
echo "--- Horizontal Pod Autoscalers ---"
for app in "api-gateway" "order-service"; do
  HPA_EXISTS=$(kubectl get hpa -n ecommerce "${app}-hpa" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  check "$([ "$HPA_EXISTS" -gt 0 ] && echo true)" "HPA exists: ${app}-hpa"
done

# ---- PDB ----
echo ""
echo "--- Pod Disruption Budgets ---"
for app in "api-gateway" "user-service" "product-service" "order-service" "notification-service"; do
  PDB_EXISTS=$(kubectl get pdb -n ecommerce "${app}-pdb" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  check "$([ "$PDB_EXISTS" -gt 0 ] && echo true)" "PDB exists: ${app}-pdb"
done

# ---- NETWORK POLICIES ----
echo ""
echo "--- Network Policies ---"
for ns in "ecommerce" "ecommerce-data" "observability"; do
  NP_COUNT=$(kubectl get networkpolicy -n "$ns" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  check "$([ "$NP_COUNT" -gt 0 ] && echo true)" "NetworkPolicy in $ns -> count: $NP_COUNT"
done

# ---- SECRETS ----
echo ""
echo "--- Secrets ---"
SECRET_EXISTS=$(kubectl get secret freshcart-secrets -n ecommerce --no-headers 2>/dev/null | wc -l | tr -d ' ')
check "$([ "$SECRET_EXISTS" -gt 0 ] && echo true)" "freshcart-secrets exists in ecommerce namespace"

# ---- SERVICES ----
echo ""
echo "--- K8s Services ---"
GW_SVC_TYPE=$(kubectl get svc api-gateway -n ecommerce -o jsonpath='{.spec.type}' 2>/dev/null)
check "$([ "$GW_SVC_TYPE" = "NodePort" ] && echo true)" "api-gateway service is NodePort -> $GW_SVC_TYPE"

# ---- GATEWAY ACCESSIBLE ----
echo ""
echo "--- Gateway Accessibility ---"
GW_HEALTH=$(curl -s -o /dev/null -w "%{http_code}" "$GATEWAY/health" 2>/dev/null)
check "$([ "$GW_HEALTH" = "200" ] && echo true)" "Gateway health via NodePort -> $GW_HEALTH"

GW_READY=$(curl -s "$GATEWAY/ready" | jq -r '.status // empty' 2>/dev/null)
check "$([ "$GW_READY" = "ready" ] && echo true)" "Gateway ready via NodePort -> $GW_READY"

GW_METRICS=$(curl -s "$GATEWAY/metrics" | grep -c "http_requests_total" 2>/dev/null)
check "$([ "$GW_METRICS" -gt 0 ] && echo true)" "Gateway /metrics works -> has http_requests_total"

# ---- END-TO-END ----
echo ""
echo "--- End-to-End via K8s ---"

# Register
K8S_REG=$(curl -s -w "\n%{http_code}" -X POST "$GATEWAY/api/v1/auth/register" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$K8S_EMAIL\",\"password\":\"pass123\",\"name\":\"K8s Tester\",\"delivery_address\":\"123 K8s Ave\"}")
K8S_REG_CODE=$(echo "$K8S_REG" | tail -1)
K8S_REG_BODY=$(echo "$K8S_REG" | head -1)
K8S_TOKEN=$(echo "$K8S_REG_BODY" | jq -r '.token // empty' 2>/dev/null)
K8S_USER_ID=$(echo "$K8S_REG_BODY" | jq -r '.id // empty' 2>/dev/null)
check "$([ "$K8S_REG_CODE" = "201" ] && echo true)" "Register user -> $K8S_REG_CODE"
check "$([ -n "$K8S_TOKEN" ] && echo true)" "Got JWT token"

# Login
K8S_LOGIN_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY/api/v1/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$K8S_EMAIL\",\"password\":\"pass123\"}")
check "$([ "$K8S_LOGIN_CODE" = "200" ] && echo true)" "Login -> $K8S_LOGIN_CODE"

# Auth: protected route without token
NO_AUTH=$(curl -s -o /dev/null -w "%{http_code}" "$GATEWAY/api/v1/products")
check "$([ "$NO_AUTH" = "401" ] && echo true)" "Protected route without token -> $NO_AUTH (expect 401)"

# Create product
K8S_PROD=$(curl -s -w "\n%{http_code}" -X POST "$GATEWAY/api/v1/products" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $K8S_TOKEN" \
  -d '{"name":"K8s Avocado","description":"Fresh","price":5.99,"category":"produce","stock":50}')
K8S_PROD_CODE=$(echo "$K8S_PROD" | tail -1)
K8S_PROD_BODY=$(echo "$K8S_PROD" | head -1)
K8S_PROD_ID=$(echo "$K8S_PROD_BODY" | jq -r '.id // empty' 2>/dev/null)
check "$([ "$K8S_PROD_CODE" = "201" ] && echo true)" "Create product -> $K8S_PROD_CODE"

# Get product
K8S_GET_PROD=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $K8S_TOKEN" "$GATEWAY/api/v1/products/$K8S_PROD_ID")
check "$([ "$K8S_GET_PROD" = "200" ] && echo true)" "Get product -> $K8S_GET_PROD"

# Place order
K8S_ORDER=$(curl -s -w "\n%{http_code}" -X POST "$GATEWAY/api/v1/orders" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $K8S_TOKEN" \
  -H "X-Correlation-ID: k8s-e2e-$(date +%s)" \
  -d "{\"user_id\":\"$K8S_USER_ID\",\"delivery_address\":\"123 K8s Ave\",\"items\":[{\"product_id\":\"$K8S_PROD_ID\",\"quantity\":2}]}")
K8S_ORDER_CODE=$(echo "$K8S_ORDER" | tail -1)
K8S_ORDER_BODY=$(echo "$K8S_ORDER" | head -1)
K8S_ORDER_ID=$(echo "$K8S_ORDER_BODY" | jq -r '.id // empty' 2>/dev/null)
K8S_ORDER_STATUS=$(echo "$K8S_ORDER_BODY" | jq -r '.status // empty' 2>/dev/null)
check "$([ "$K8S_ORDER_CODE" = "201" ] || [ "$K8S_ORDER_CODE" = "200" ] && echo true)" "Place order -> $K8S_ORDER_CODE"
check "$([ "$K8S_ORDER_STATUS" = "confirmed" ] && echo true)" "Order confirmed -> $K8S_ORDER_STATUS"

# Stock decremented
K8S_STOCK=$(curl -s -H "Authorization: Bearer $K8S_TOKEN" "$GATEWAY/api/v1/products/$K8S_PROD_ID" | jq -r '.stock // empty' 2>/dev/null)
check "$([ "$K8S_STOCK" = "48" ] && echo true)" "Stock decremented -> $K8S_STOCK (expect 48)"

# Get order
K8S_GET_ORDER=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $K8S_TOKEN" "$GATEWAY/api/v1/orders/$K8S_ORDER_ID")
check "$([ "$K8S_GET_ORDER" = "200" ] && echo true)" "Get order -> $K8S_GET_ORDER"

# Order state machine
K8S_PACK=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$GATEWAY/api/v1/orders/$K8S_ORDER_ID/status" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $K8S_TOKEN" \
  -d '{"status":"packing"}')
check "$([ "$K8S_PACK" = "200" ] && echo true)" "Order -> packing -> $K8S_PACK"

K8S_OFD=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$GATEWAY/api/v1/orders/$K8S_ORDER_ID/status" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $K8S_TOKEN" \
  -d '{"status":"out_for_delivery"}')
check "$([ "$K8S_OFD" = "200" ] && echo true)" "Order -> out_for_delivery -> $K8S_OFD"

K8S_DELIVERED=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$GATEWAY/api/v1/orders/$K8S_ORDER_ID/status" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $K8S_TOKEN" \
  -d '{"status":"delivered"}')
check "$([ "$K8S_DELIVERED" = "200" ] && echo true)" "Order -> delivered -> $K8S_DELIVERED"

# Invalid transition
K8S_INVALID=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$GATEWAY/api/v1/orders/$K8S_ORDER_ID/status" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $K8S_TOKEN" \
  -d '{"status":"pending"}')
check "$([ "$K8S_INVALID" = "400" ] && echo true)" "Invalid transition rejected -> $K8S_INVALID (expect 400)"

# User profile
K8S_PROFILE=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $K8S_TOKEN" "$GATEWAY/api/v1/users/$K8S_USER_ID")
check "$([ "$K8S_PROFILE" = "200" ] && echo true)" "Get user profile -> $K8S_PROFILE"

# List user orders
K8S_USER_ORDERS=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $K8S_TOKEN" "$GATEWAY/api/v1/orders/user/$K8S_USER_ID")
check "$([ "$K8S_USER_ORDERS" = "200" ] && echo true)" "List user orders -> $K8S_USER_ORDERS"

# Notifications (async - wait)
sleep 5
K8S_NOTIFS=$(curl -s "$GATEWAY/api/v1/notifications/user/$K8S_USER_ID" -H "Authorization: Bearer $K8S_TOKEN")
K8S_NOTIF_COUNT=$(echo "$K8S_NOTIFS" | jq 'length' 2>/dev/null)
check "$([ "$K8S_NOTIF_COUNT" -gt 0 ] && echo true)" "Notifications received -> count: $K8S_NOTIF_COUNT"

# ---- OBSERVABILITY ----
echo ""
echo "--- Observability via K8s ---"

GRAFANA_HEALTH=$(curl -s -o /dev/null -w "%{http_code}" "$GRAFANA/api/health" 2>/dev/null)
check "$([ "$GRAFANA_HEALTH" = "200" ] && echo true)" "Grafana accessible -> $GRAFANA_HEALTH"

GRAFANA_DS=$(curl -s "$GRAFANA/api/datasources" | jq 'length' 2>/dev/null)
check "$([ "$GRAFANA_DS" -ge 3 ] && echo true)" "Grafana has >=3 datasources -> $GRAFANA_DS"

GRAFANA_DASH=$(curl -s "$GRAFANA/api/search" | jq 'length' 2>/dev/null)
check "$([ "$GRAFANA_DASH" -ge 3 ] && echo true)" "Grafana has >=3 dashboards -> $GRAFANA_DASH"

JAEGER_HEALTH=$(curl -s -o /dev/null -w "%{http_code}" "$JAEGER/" 2>/dev/null)
check "$([ "$JAEGER_HEALTH" = "200" ] && echo true)" "Jaeger accessible -> $JAEGER_HEALTH"

# Jaeger has service traces (wait for traces to export)
sleep 5
JAEGER_SVCS=$(curl -s "$JAEGER/api/services" | jq '.data | length' 2>/dev/null)
check "$([ "$JAEGER_SVCS" -gt 0 ] && echo true)" "Jaeger has service traces -> $JAEGER_SVCS services"

# ---- STRUCTURED LOGGING IN K8S ----
echo ""
echo "--- Structured Logging in K8s ---"

GW_LOG=$(kubectl logs -n ecommerce -l app=api-gateway --tail 1 2>/dev/null)
GW_LOG_JSON=$(echo "$GW_LOG" | head -1 | jq . >/dev/null 2>&1 && echo "true" || echo "false")
check "$([ "$GW_LOG_JSON" = "true" ] && echo true)" "Gateway logs are JSON in K8s"

CID_IN_K8S_LOGS=$(kubectl logs -n ecommerce -l app=api-gateway --tail 20 2>/dev/null | grep -c "correlation_id")
check "$([ "$CID_IN_K8S_LOGS" -gt 0 ] && echo true)" "K8s logs contain correlation_id -> $CID_IN_K8S_LOGS"

# ---- PROMETHEUS SCRAPING IN K8S ----
echo ""
echo "--- Prometheus in K8s ---"

# Port-forward prometheus briefly to check targets
PROM_POD=$(kubectl get pods -n observability -l app=prometheus -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -n "$PROM_POD" ]; then
  kubectl port-forward -n observability "$PROM_POD" 9091:9090 &>/dev/null &
  PF_PID=$!
  sleep 3
  PROM_TARGETS=$(curl -s "http://localhost:9091/api/v1/targets" | jq '.data.activeTargets | length' 2>/dev/null)
  check "$([ "$PROM_TARGETS" -gt 0 ] && echo true)" "Prometheus scraping targets -> $PROM_TARGETS"

  PROM_ALERTS=$(curl -s "http://localhost:9091/api/v1/rules" | jq '[.data.groups[].rules[]] | length' 2>/dev/null)
  check "$([ "$PROM_ALERTS" -ge 4 ] && echo true)" "Prometheus has >=4 alert rules -> $PROM_ALERTS"

  kill $PF_PID 2>/dev/null
  wait $PF_PID 2>/dev/null
fi

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
