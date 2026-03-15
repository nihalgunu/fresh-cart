# FreshCart — Online Grocery Delivery Platform

A microservices-based backend for an online grocery delivery startup, built with Go, Docker, and Kubernetes.

## Quick Start
```bash
# Docker Compose
make build && make up && sleep 20 && make seed

# Kubernetes
make boot
```

## Documentation
- [Architecture](docs/ARCHITECTURE.md) — System design, service descriptions, communication patterns
- [Setup Guide](docs/SETUP.md) — Prerequisites, deployment instructions, make targets

## Test Suite
```bash
make test-all    # Docker Compose integration tests
make test-k8s    # Kubernetes integration tests
make chaos       # Chaos testing
make loadtest    # Load testing
make trivy       # Security scanning
```

## Services
| Service | Port | Database |
|---------|------|----------|
| API Gateway | 8080 | Redis |
| User Service | 8081 | PostgreSQL |
| Product Service | 8082 | PostgreSQL |
| Order Service | 8083 | PostgreSQL |
| Notification Service | 8084 | MongoDB |
