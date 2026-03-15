.PHONY: build up down logs seed test-e2e clean boot boot-compose test-all test-k8s open-grafana trivy chaos loadtest
.PHONY: boot-kind boot-images boot-k8s boot-wait boot-down boot-k8s-full health-k8s

build:
	docker compose build

up:
	docker compose up -d

# Docker Compose quick start (build + up + health)
boot-compose: build up
	@echo "Waiting for services to be ready..."
	@sleep 20
	@$(MAKE) health

# Kubernetes (Kind): create cluster, build images, deploy to K8s
boot: boot-k8s-full

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

test-all:
	@bash scripts/test-all.sh

trivy:
	@bash scripts/trivy-scan.sh

chaos:
	@bash scripts/chaos.sh

loadtest:
	@bash scripts/loadtest.sh

test-k8s:
	@bash scripts/test-k8s.sh

open-grafana:
	@echo "Grafana: http://localhost:3000 (admin/admin)"
	@echo "Prometheus: http://localhost:9090"
	@echo "Jaeger: http://localhost:16686"

# =============================================================================
# Kubernetes / Kind Deployment
# =============================================================================

# The main command - boots everything from zero on Kind
boot-k8s-full: boot-kind boot-images boot-k8s boot-wait
	@echo ""
	@echo "============================================"
	@echo "  FreshCart is running on Kind!"
	@echo "============================================"
	@echo "  API Gateway: http://localhost:30080"
	@echo "  Grafana:     http://localhost:30030 (admin/admin)"
	@echo "  Jaeger:      http://localhost:30086"
	@echo "  RabbitMQ:    http://localhost:30072"
	@echo "============================================"

# Step 1: Create Kind cluster if it doesn't exist
boot-kind:
	@echo "--- Creating Kind cluster ---"
	@kind get clusters 2>/dev/null | grep -q freshcart || kind create cluster --name freshcart --config k8s/kind-config.yml
	@kubectl cluster-info --context kind-freshcart

# Step 2: Build and load images into Kind
boot-images:
	@echo "--- Building Docker images ---"
	docker compose build
	@echo "--- Loading images into Kind ---"
	kind load docker-image freshcart-api-gateway:latest --name freshcart
	kind load docker-image freshcart-user-service:latest --name freshcart
	kind load docker-image freshcart-product-service:latest --name freshcart
	kind load docker-image freshcart-order-service:latest --name freshcart
	kind load docker-image freshcart-notification-service:latest --name freshcart

# Step 3: Apply all K8s manifests
boot-k8s:
	@echo "--- Applying Kubernetes manifests ---"
	kubectl apply -f k8s/namespaces.yml
	kubectl apply -f k8s/ecommerce-data/ --recursive
	kubectl apply -f k8s/ecommerce/ --recursive
	kubectl apply -f k8s/observability/ --recursive

# Step 4: Wait for everything to be ready
boot-wait:
	@echo "--- Waiting for pods to be ready ---"
	kubectl wait --for=condition=ready pod -l app=user-db -n ecommerce-data --timeout=120s 2>/dev/null || true
	kubectl wait --for=condition=ready pod -l app=product-db -n ecommerce-data --timeout=120s 2>/dev/null || true
	kubectl wait --for=condition=ready pod -l app=order-db -n ecommerce-data --timeout=120s 2>/dev/null || true
	kubectl wait --for=condition=ready pod -l app=redis -n ecommerce-data --timeout=120s 2>/dev/null || true
	kubectl wait --for=condition=ready pod -l app=rabbitmq -n ecommerce-data --timeout=120s 2>/dev/null || true
	kubectl wait --for=condition=ready pod -l app=notification-db -n ecommerce-data --timeout=120s 2>/dev/null || true
	@echo "--- Data services ready, waiting for app services ---"
	kubectl wait --for=condition=ready pod --all -n ecommerce --timeout=180s 2>/dev/null || true
	kubectl wait --for=condition=ready pod --all -n observability --timeout=180s 2>/dev/null || true
	@echo "--- All pods ready ---"

# Tear down Kind cluster
boot-down:
	kind delete cluster --name freshcart

# K8s health check
health-k8s:
	@echo "=== K8s Health checks ==="
	@curl -s http://localhost:30080/health 2>/dev/null | jq . || echo "Gateway not ready"
	@echo ""
	@echo "=== Pod Status ==="
	@kubectl get pods -n ecommerce
	@echo ""
	@kubectl get pods -n ecommerce-data
	@echo ""
	@kubectl get pods -n observability
