#!/bin/bash
set -e

GATEWAY="http://localhost:8000"

echo "=== Seeding FreshCart ==="

echo ""
echo "--- Registering user ---"
USER_RESPONSE=$(curl -s -X POST "$GATEWAY/api/v1/auth/register" \
  -H "Content-Type: application/json" \
  -d '{"email":"alice@freshcart.com","password":"password123","name":"Alice Smith","delivery_address":"123 Avocado Lane, San Francisco, CA"}')
echo "$USER_RESPONSE" | jq .
USER_ID=$(echo "$USER_RESPONSE" | jq -r '.id')
TOKEN=$(echo "$USER_RESPONSE" | jq -r '.token')

echo ""
echo "--- Creating products ---"

PRODUCTS=(
  '{"name":"Organic Avocados (4 pack)","description":"Ripe Hass avocados from Mexico","price":5.99,"category":"produce","stock":100,"image_url":""}'
  '{"name":"Organic Whole Milk 1gal","description":"Locally sourced organic whole milk","price":6.49,"category":"dairy","stock":50,"image_url":""}'
  '{"name":"Weekend BBQ Kit","description":"Burgers, buns, condiments, and sides for 4","price":29.99,"category":"meal_kits","stock":25,"image_url":""}'
  '{"name":"Sourdough Bread Loaf","description":"Freshly baked artisan sourdough","price":7.99,"category":"bakery","stock":40,"image_url":""}'
  '{"name":"Fresh Atlantic Salmon 1lb","description":"Wild-caught Atlantic salmon fillet","price":14.99,"category":"seafood","stock":30,"image_url":""}'
)

PRODUCT_IDS=()
for product in "${PRODUCTS[@]}"; do
  RESULT=$(curl -s -X POST "$GATEWAY/api/v1/products" \
    -H "Content-Type: application/json" \
    -d "$product")
  echo "$RESULT" | jq '{id: .id, name: .name, stock: .stock}'
  PRODUCT_IDS+=($(echo "$RESULT" | jq -r '.id'))
done

echo ""
echo "=== Seed Complete ==="
echo ""
echo "USER_ID=$USER_ID"
echo "TOKEN=$TOKEN"
echo ""
echo "Product IDs:"
for i in "${!PRODUCT_IDS[@]}"; do
  echo "  [$i] ${PRODUCT_IDS[$i]}"
done
echo ""
echo "Example order command:"
echo "curl -s -X POST $GATEWAY/api/v1/orders \\"
echo "  -H 'Content-Type: application/json' \\"
echo "  -d '{\"user_id\":\"$USER_ID\",\"delivery_address\":\"123 Avocado Lane\",\"items\":[{\"product_id\":\"${PRODUCT_IDS[0]}\",\"quantity\":2},{\"product_id\":\"${PRODUCT_IDS[2]}\",\"quantity\":1}]}' | jq ."
