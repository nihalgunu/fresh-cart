.PHONY: build up down logs seed test-e2e clean boot

build:
	docker compose build

up:
	docker compose up -d

boot: build up
	@echo "Waiting for services to be ready..."
	@sleep 20
	@$(MAKE) health

down:
	docker compose down

logs:
	docker compose logs -f

seed:
	@bash scripts/seed.sh

clean:
	docker compose down -v

health:
	@echo "=== Health checks ==="
	@curl -s http://localhost:8000/health 2>/dev/null | jq . || echo "Gateway not ready"
	@curl -s http://localhost:8081/health 2>/dev/null | jq . || echo "User service not ready"
	@curl -s http://localhost:8082/health 2>/dev/null | jq . || echo "Product service not ready"
	@curl -s http://localhost:8083/health 2>/dev/null | jq . || echo "Order service not ready"
	@curl -s http://localhost:8084/health 2>/dev/null | jq . || echo "Notification service not ready"

test-e2e:
	@echo "=== Health checks ==="
	@curl -s http://localhost:8000/health | jq .
	@curl -s http://localhost:8081/health | jq .
	@curl -s http://localhost:8082/health | jq .
	@curl -s http://localhost:8083/health | jq .
	@curl -s http://localhost:8084/health | jq .
	@echo ""
	@echo "=== Register + Login ==="
	@curl -s -X POST http://localhost:8000/api/v1/auth/register \
		-H "Content-Type: application/json" \
		-d '{"email":"test@test.com","password":"test1234","name":"Test User","delivery_address":"456 Test St"}' | jq .
	@echo ""
	@echo "=== Create a product ==="
	@curl -s -X POST http://localhost:8000/api/v1/products \
		-H "Content-Type: application/json" \
		-d '{"name":"Test Avocados","description":"Test","price":4.99,"category":"produce","stock":10}' | jq .
	@echo ""
	@echo "Run 'make seed' to seed data, then place an order with the IDs"
