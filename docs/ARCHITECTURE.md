# FreshCart Architecture

## Overview
FreshCart is an online grocery delivery platform built as a set of 5 microservices, each with its own database, running in Docker containers and deployed to a local Kubernetes cluster.

## Services

### API Gateway (port 8080)
- **Role**: Front door for all client requests
- **Database**: Redis (rate limiting, caching)
- **Key features**: JWT auth validation, rate limiting (token bucket via Redis), reverse proxy routing, correlation ID generation
- **Routes**:
  - Public: POST /api/v1/auth/register, POST /api/v1/auth/login
  - Protected: /api/v1/users/*, /api/v1/products/*, /api/v1/orders/*, /api/v1/notifications/*

### User Service (port 8081)
- **Role**: Customer account management
- **Database**: PostgreSQL (freshcart_users)
- **Key features**: Registration with bcrypt hashing, JWT token generation, user profile management

### Product Service (port 8082)
- **Role**: Grocery catalog and inventory management
- **Database**: PostgreSQL (freshcart_products)
- **Key features**: Product CRUD, search with filtering, stock management, RabbitMQ consumer for async inventory updates

### Order Service (port 8083)
- **Role**: Order placement and lifecycle management
- **Database**: PostgreSQL (freshcart_orders)
- **Key features**: Saga orchestration (reserve → pay → confirm), compensating transactions, state machine (pending → confirmed → packing → out_for_delivery → delivered / cancelled), RabbitMQ publisher

### Notification Service (port 8084)
- **Role**: Event-driven alerts
- **Database**: MongoDB (freshcart_notifications)
- **Key features**: Consumes order events from RabbitMQ, stores notifications, simulates email delivery

## Communication Patterns

### Synchronous (REST)
```
Client → API Gateway → User Service
                     → Product Service
                     → Order Service
```

### Asynchronous (RabbitMQ)
```
Order Service → exchange: "orders" → Product Service (inventory.update queue)
                                   → Notification Service (notifications.order queue)
```

## Order Saga Flow

1. Client sends POST /api/v1/orders with items
2. Order Service calls Product Service to validate each product exists
3. Order Service calls Product Service to reserve stock for each item (PATCH /stock)
4. If any reservation fails → compensating transaction releases already-reserved items
5. Simulate payment (100ms delay)
6. If payment fails → release all reserved stock
7. Insert order into database with status "confirmed"
8. Publish order.confirmed event to RabbitMQ
9. Notification Service stores confirmation notification
10. Product Service updates inventory via async consumer

## State Machine
```
pending → confirmed → packing → out_for_delivery → delivered
                    → cancelled (releases reserved stock)
          failed (saga failure)
```

## Resilience Patterns

- **Circuit Breaker**: Order Service → Product Service (5 failure threshold, 10s open timeout)
- **Retry**: Exponential backoff with jitter (3 retries, 100ms initial, 2s max)
- **Timeouts**: 5 seconds on all outgoing HTTP calls
- **Bulkheading**: Semaphore limiting concurrent saga executions to 10
- **Dead Letter Queues**: Failed messages routed to .dlq queues instead of being lost
- **Rate Limiting**: Redis-based sliding window on API Gateway

## Observability Stack

- **Logging**: Structured JSON via Go slog → Promtail → Loki → Grafana
- **Metrics**: Prometheus client in each service → Prometheus server → Grafana
- **Tracing**: OpenTelemetry SDK → Jaeger (OTLP over HTTP)
- **Correlation ID**: Generated at gateway, propagated via X-Correlation-ID header across REST and RabbitMQ

## Kubernetes Architecture

- **ecommerce namespace**: All 5 application services (2+ replicas each)
- **ecommerce-data namespace**: PostgreSQL (x3), MongoDB, Redis, RabbitMQ
- **observability namespace**: Prometheus, Loki, Promtail (DaemonSet), Grafana, Jaeger
- **HPA**: API Gateway and Order Service (scale on 70% CPU)
- **PDB**: All services (minAvailable: 1)
- **NetworkPolicies**: Namespace isolation (ecommerce-data only accepts from ecommerce)

## Grafana Dashboards

- **Service Overview**: RED metrics (Rate, Errors, Duration) for all services
- **Business Metrics**: Orders, inventory levels, registrations, notifications
- **Logs Explorer**: Filter logs by service and correlation ID via Loki

## Alert Rules

- **HighErrorRate**: >5% error rate for 2 minutes
- **ServiceDown**: Service unreachable for 1 minute
- **HighLatency**: p95 latency >2 seconds for 5 minutes
- **LowInventory**: Stock level below 5 units for 1 minute
