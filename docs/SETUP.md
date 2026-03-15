# FreshCart Setup Guide

## Prerequisites
- Docker and Docker Compose
- Go 1.22+ (optional, for local development)
- Kind (for Kubernetes deployment)
- kubectl
- jq (for test scripts)

## Quick Start (Docker Compose)
```bash
# Option A: one command (build + up + health)
make boot-compose

# Option B: step by step
# Build all services
make build

# Start everything
make up

# Wait for services, then seed test data
sleep 20
make seed

# Run full test suite
make test-all

# Run chaos tests
make chaos

# Run load tests
make loadtest

# View logs
make logs

# Stop everything
make down

# Stop and remove all data
make clean
```

## Kubernetes Deployment
```bash
# Boot from zero (creates Kind cluster, builds images, deploys everything)
make boot

# Run Kubernetes test suite
make test-k8s

# Tear down
make boot-down
```

## Accessing Services

### Docker Compose
| Service | URL |
|---------|-----|
| API Gateway | http://localhost:8000 |
| Grafana | http://localhost:3000 (admin/admin) |
| Prometheus | http://localhost:9090 |
| Jaeger | http://localhost:16686 |
| RabbitMQ Management | http://localhost:15672 (guest/guest) |

### Kubernetes (Kind)
| Service | URL |
|---------|-----|
| API Gateway | http://localhost:30080 |
| Grafana | http://localhost:30030 (admin/admin) |
| Jaeger | http://localhost:30086 |

## End-to-End Test Flow
```bash
# 1. Register a user
curl -X POST http://localhost:8000/api/v1/auth/register \
  -H "Content-Type: application/json" \
  -d '{"email":"demo@freshcart.com","password":"demo123","name":"Demo User","delivery_address":"123 Demo St"}'

# 2. Create a product
curl -X POST http://localhost:8000/api/v1/products \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <TOKEN>" \
  -d '{"name":"Organic Avocados","price":5.99,"category":"produce","stock":100}'

# 3. Place an order
curl -X POST http://localhost:8000/api/v1/orders \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <TOKEN>" \
  -H "X-Correlation-ID: demo-order-001" \
  -d '{"user_id":"<USER_ID>","delivery_address":"123 Demo St","items":[{"product_id":"<PRODUCT_ID>","quantity":2}]}'

# 4. Check Grafana dashboard: http://localhost:3000
# 5. Check Jaeger trace: http://localhost:16686 (search for api-gateway)
# 6. Check logs in Grafana: Explore → Loki → {service=~".+"} |= "demo-order-001"
```

## Make Targets
| Target | Description |
|--------|-------------|
| make build | Build all Docker images |
| make up | Start docker compose |
| make down | Stop docker compose |
| make clean | Stop and remove all volumes |
| make logs | Tail all service logs |
| make seed | Seed test data |
| make test-all | Run full integration test suite |
| make test-k8s | Run Kubernetes test suite |
| make chaos | Run chaos testing scenarios |
| make loadtest | Run load test scenarios |
| make boot | Deploy to Kind from zero |
| make boot-down | Tear down Kind cluster |
| make trivy | Run security scans |
