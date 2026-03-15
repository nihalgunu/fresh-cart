#!/bin/bash
echo "============================================"
echo "  FreshCart Security Scan (Trivy)"
echo "============================================"
echo ""

# Check if trivy is installed
if ! command -v trivy &> /dev/null; then
  echo "Trivy not installed. Installing via Docker..."
  TRIVY_CMD="docker run --rm -v /var/run/docker.sock:/var/run/docker.sock aquasec/trivy:latest image"
else
  TRIVY_CMD="trivy image"
fi

IMAGES=(
  "freshcart-api-gateway:latest"
  "freshcart-user-service:latest"
  "freshcart-product-service:latest"
  "freshcart-order-service:latest"
  "freshcart-notification-service:latest"
)

OVERALL_EXIT=0

for img in "${IMAGES[@]}"; do
  echo ""
  echo "--- Scanning $img ---"
  $TRIVY_CMD --severity HIGH,CRITICAL --exit-code 0 --no-progress "$img" 2>/dev/null
  if [ $? -ne 0 ]; then
    echo "  WARNING: Scan failed for $img"
    OVERALL_EXIT=1
  fi
done

echo ""
echo "============================================"
echo "  Scan Complete"
echo "============================================"
exit $OVERALL_EXIT
