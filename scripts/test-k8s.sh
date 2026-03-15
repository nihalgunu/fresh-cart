#!/bin/bash
# FreshCart Kubernetes Integration Test Suite
# Tests Kubernetes-specific resources across 3 namespaces: ecommerce, ecommerce-data, observability

set -o pipefail

# Configuration
GATEWAY="${GATEWAY_URL:-http://localhost:30080}"
GRAFANA="${GRAFANA_URL:-http://localhost:30030}"
JAEGER="${JAEGER_URL:-http://localhost:30086}"

# Counters
PASS=0
FAIL=0

green() { echo -e "\033[32m  PASS: $1\033[0m"; PASS=$((PASS+1)); }
red() { echo -e "\033[31m  FAIL: $1\033[0m"; FAIL=$((FAIL+1)); }
check() {
  if [ "$1" = "true" ]; then green "$2"; else red "$2"; fi
}

echo "============================================"
echo "  FreshCart Kubernetes Test Suite"
echo "============================================"
echo "  Gateway: $GATEWAY"
echo "  Grafana: $GRAFANA"
echo "  Jaeger:  $JAEGER"
echo ""

# ---- NAMESPACES ----
echo "--- Namespaces ---"
for ns in ecommerce ecommerce-data observability; do
  EXISTS=$(kubectl get namespace "$ns" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  check "$([ "$EXISTS" -eq 1 ] && echo true)" "Namespace '$ns' exists"
done

# ---- ECOMMERCE PODS (≥2 Running each) ----
echo ""
echo "--- Ecommerce Pods (≥2 Running) ---"
ECOMMERCE_SERVICES="api-gateway user-service product-service order-service notification-service"
for svc in $ECOMMERCE_SERVICES; do
  RUNNING=$(kubectl -n ecommerce get pods -l app="$svc" --no-headers 2>/dev/null | grep -c "Running" || echo "0")
  check "$([ "$RUNNING" -ge 2 ] && echo true)" "$svc has ≥2 Running pods → $RUNNING"
done

# ---- DATA PODS (≥1 Running each) ----
echo ""
echo "--- Data Pods (≥1 Running) ---"
DATA_SERVICES="user-db product-db order-db notification-db redis rabbitmq"
for svc in $DATA_SERVICES; do
  RUNNING=$(kubectl -n ecommerce-data get pods -l app="$svc" --no-headers 2>/dev/null | grep -c "Running" || echo "0")
  check "$([ "$RUNNING" -ge 1 ] && echo true)" "$svc has ≥1 Running pod → $RUNNING"
done

# ---- OBSERVABILITY PODS (≥1 Running each) ----
echo ""
echo "--- Observability Pods (≥1 Running) ---"
OBSERVABILITY_SERVICES="prometheus loki grafana jaeger"
for svc in $OBSERVABILITY_SERVICES; do
  RUNNING=$(kubectl -n observability get pods -l app="$svc" --no-headers 2>/dev/null | grep -c "Running" || echo "0")
  check "$([ "$RUNNING" -ge 1 ] && echo true)" "$svc has ≥1 Running pod → $RUNNING"
done

# Promtail (separate check)
PROMTAIL_RUNNING=$(kubectl -n observability get pods -l app=promtail --no-headers 2>/dev/null | grep -c "Running" || echo "0")
check "$([ "$PROMTAIL_RUNNING" -ge 1 ] && echo true)" "promtail has ≥1 Running pod → $PROMTAIL_RUNNING"

# ---- HORIZONTAL POD AUTOSCALERS ----
echo ""
echo "--- Horizontal Pod Autoscalers ---"
for hpa in api-gateway-hpa order-service-hpa; do
  EXISTS=$(kubectl -n ecommerce get hpa "$hpa" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  check "$([ "$EXISTS" -eq 1 ] && echo true)" "HPA '$hpa' exists in ecommerce namespace"
done

# ---- POD DISRUPTION BUDGETS ----
echo ""
echo "--- Pod Disruption Budgets ---"
for svc in $ECOMMERCE_SERVICES; do
  PDB_EXISTS=$(kubectl -n ecommerce get pdb "${svc}-pdb" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  check "$([ "$PDB_EXISTS" -eq 1 ] && echo true)" "PDB '${svc}-pdb' exists in ecommerce namespace"
done

# ---- NETWORK POLICIES ----
echo ""
echo "--- Network Policies ---"
for ns in ecommerce ecommerce-data observability; do
  NETPOL_COUNT=$(kubectl -n "$ns" get networkpolicy --no-headers 2>/dev/null | wc -l | tr -d ' ')
  check "$([ "$NETPOL_COUNT" -ge 1 ] && echo true)" "Namespace '$ns' has ≥1 NetworkPolicy → $NETPOL_COUNT"
done

# ---- SECRETS ----
echo ""
echo "--- Secrets ---"
SECRET_EXISTS=$(kubectl -n ecommerce get secret freshcart-secrets --no-headers 2>/dev/null | wc -l | tr -d ' ')
check "$([ "$SECRET_EXISTS" -eq 1 ] && echo true)" "Secret 'freshcart-secrets' exists in ecommerce namespace"

# ---- SERVICES ----
echo ""
echo "--- Services ---"
SVC_TYPE=$(kubectl -n ecommerce get svc api-gateway -o jsonpath='{.spec.type}' 2>/dev/null)
check "$([ "$SVC_TYPE" = "NodePort" ] && echo true)" "api-gateway service is NodePort → $SVC_TYPE"

# ---- GATEWAY ACCESSIBILITY ----
echo ""
echo "--- Gateway Accessibility ---"
GW_HEALTH=$(curl -s -o /dev/null -w "%{http_code}" "$GATEWAY/health" 2>/dev/null || echo "000")
check "$([ "$GW_HEALTH" = "200" ] && echo true)" "Gateway health check returns 200 → $GW_HEALTH"

# ---- END-TO-END TESTS ----
echo ""
echo "--- End-to-End Tests ---"

UNIQUE_ID=$(date +%s)

# Register user
REG_RESPONSE=$(curl -s -X POST "$GATEWAY/api/v1/auth/register" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"k8stest-$UNIQUE_ID@test.com\",\"password\":\"test123\",\"name\":\"K8s Test\",\"delivery_address\":\"K8s Cluster\"}" 2>/dev/null)
TOKEN=$(echo "$REG_RESPONSE" | jq -r '.token // empty' 2>/dev/null)
USER_ID=$(echo "$REG_RESPONSE" | jq -r '.id // empty' 2>/dev/null)
check "$([ -n "$TOKEN" ] && echo true)" "Register user through gateway"

# Create product
if [ -n "$TOKEN" ]; then
  PROD_RESPONSE=$(curl -s -X POST "$GATEWAY/api/v1/products" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $TOKEN" \
    -d '{"name":"K8s Test Product","description":"Test","price":9.99,"category":"test","stock":50}' 2>/dev/null)
  PROD_ID=$(echo "$PROD_RESPONSE" | jq -r '.id // empty' 2>/dev/null)
  check "$([ -n "$PROD_ID" ] && echo true)" "Create product through gateway"

  # Place order
  if [ -n "$PROD_ID" ] && [ -n "$USER_ID" ]; then
    ORDER_RESPONSE=$(curl -s -X POST "$GATEWAY/api/v1/orders" \
      -H "Content-Type: application/json" \
      -H "Authorization: Bearer $TOKEN" \
      -d "{\"user_id\":\"$USER_ID\",\"delivery_address\":\"123 K8s St\",\"items\":[{\"product_id\":\"$PROD_ID\",\"quantity\":2}]}" 2>/dev/null)
    ORDER_STATUS=$(echo "$ORDER_RESPONSE" | jq -r '.status // empty' 2>/dev/null)
    check "$([ "$ORDER_STATUS" = "confirmed" ] && echo true)" "Place order through gateway → status: $ORDER_STATUS"
  else
    red "Place order through gateway (no product ID)"
  fi
else
  red "Create product through gateway (no token)"
  red "Place order through gateway (no token)"
fi

# ---- GRAFANA ACCESSIBILITY ----
echo ""
echo "--- Grafana Accessibility ---"
GRAFANA_HEALTH=$(curl -s -o /dev/null -w "%{http_code}" "$GRAFANA/api/health" 2>/dev/null || echo "000")
check "$([ "$GRAFANA_HEALTH" = "200" ] && echo true)" "Grafana health check returns 200 → $GRAFANA_HEALTH"

# Grafana datasources
GRAFANA_DS_COUNT=$(curl -s "$GRAFANA/api/datasources" 2>/dev/null | jq 'length' 2>/dev/null || echo "0")
check "$([ "$GRAFANA_DS_COUNT" -ge 3 ] && echo true)" "Grafana has ≥3 datasources → $GRAFANA_DS_COUNT"

# Grafana dashboards
GRAFANA_DB_COUNT=$(curl -s "$GRAFANA/api/search?type=dash-db" 2>/dev/null | jq 'length' 2>/dev/null || echo "0")
check "$([ "$GRAFANA_DB_COUNT" -ge 3 ] && echo true)" "Grafana has ≥3 dashboards → $GRAFANA_DB_COUNT"

# ---- JAEGER ACCESSIBILITY ----
echo ""
echo "--- Jaeger Accessibility ---"
JAEGER_HEALTH=$(curl -s -o /dev/null -w "%{http_code}" "$JAEGER/" 2>/dev/null || echo "000")
check "$([ "$JAEGER_HEALTH" = "200" ] && echo true)" "Jaeger UI returns 200 → $JAEGER_HEALTH"

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
