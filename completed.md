# FreshCart - Completed Features

## Phase 1: Core Microservices Architecture

### Services Implemented
- **API Gateway** (port 8000) - Reverse proxy routing to downstream services
- **User Service** (port 8081) - Registration, login, JWT authentication
- **Product Service** (port 8082) - Product catalog CRUD, inventory management
- **Order Service** (port 8083) - Order placement, saga orchestration
- **Notification Service** (port 8084) - Event-driven notifications via RabbitMQ

### Infrastructure
- PostgreSQL databases for User, Product, and Order services
- MongoDB for Notification service
- RabbitMQ for async messaging between services
- Docker Compose orchestration for all services
- Multi-stage Alpine Dockerfiles with non-root user

### API Endpoints
- `POST /api/v1/auth/register` - User registration
- `POST /api/v1/auth/login` - User login, returns JWT
- `GET /api/v1/users/:id` - Get user profile
- `GET /api/v1/products` - List all products
- `GET /api/v1/products/:id` - Get product details
- `POST /api/v1/products` - Create product
- `PATCH /api/v1/products/:id/stock` - Update stock
- `POST /api/v1/orders` - Place order (runs saga)
- `GET /api/v1/orders/:id` - Get order details
- `GET /api/v1/orders/user/:user_id` - List user's orders
- `GET /api/v1/notifications/user/:user_id` - List notifications

### Order Saga Pattern
- Create order (status: PENDING)
- Reserve inventory via Product Service
- Simulate payment (100ms delay)
- Confirm order (status: CONFIRMED)
- Publish `order.confirmed` event to RabbitMQ
- Notification Service consumes event, stores in MongoDB
- Compensation logic releases reserved stock on failure

---

## Phase 2: Structured Logging & Observability Foundation

### Structured JSON Logging (slog)
- All services use Go's `log/slog` with JSON handler
- Every log line is valid JSON with fields:
  - `time` - ISO 8601 timestamp
  - `level` - INFO, WARN, ERROR
  - `msg` - Log message
  - `service` - Service name identifier
  - `correlation_id` - Request trace ID

### Correlation ID Propagation
- Middleware extracts `X-Correlation-ID` header or generates UUID v4
- Correlation ID stored in request context
- Response includes `X-Correlation-ID` header
- Propagation paths:
  - API Gateway → downstream services (reverse proxy)
  - Order Service → Product Service (HTTP calls)
  - Order Service → RabbitMQ (AMQP headers)
  - Product Service ← RabbitMQ (extracts from AMQP headers)
  - Notification Service ← RabbitMQ (extracts from AMQP headers)

### Request Logging Middleware
- Logs every HTTP request with:
  - `method`, `path`, `status`, `duration_ms`
  - `correlation_id`, `service`

### Business Event Logging
- **User Service**: user registered, user logged in, login failed, migration completed
- **Product Service**: product created, stock updated, insufficient stock, inventory update received
- **Order Service**: saga started, inventory reserved, inventory reservation failed, compensating transaction, payment simulated, order confirmed, order failed, event published
- **Notification Service**: order event received, notification stored

---

## Developer Experience

### Makefile Commands
- `make boot` - Build and start all services
- `make seed` - Create test user and products
- `make health` - Check health of all services
- `make logs` - View service logs
- `make down` - Stop all services
- `make clean` - Stop and remove volumes

### Documentation
- README.md with architecture diagram, port table, API docs, testing instructions
- completed.md (this file) tracking implemented features

---

## Phase 3: Production Readiness & Observability

### Health & Readiness Endpoints
- `/health` - Liveness check on all services (returns 200 OK)
- `/ready` - Readiness check with dependency verification:
  - User Service: PostgreSQL connection
  - Product Service: PostgreSQL + RabbitMQ connection
  - Order Service: PostgreSQL + RabbitMQ connection
  - Notification Service: MongoDB + RabbitMQ connection
  - API Gateway: Redis connection

### Prometheus Metrics
- **HTTP Metrics** (all services):
  - `http_requests_total` - Request counter by method, path, status
- **Business Metrics**:
  - `users_registered_total` - User registration counter
  - `orders_created_total` - Order creation counter
  - `inventory_level` - Current stock gauge per product
  - `notifications_sent_total` - Notification counter by type

### Rate Limiting (API Gateway)
- Redis-backed sliding window rate limiter
- Configurable via environment variables:
  - `RATE_LIMIT_RPS` - Requests per second (default: 100)
  - `RATE_LIMIT_BURST` - Burst allowance (default: 200)
- Returns 429 Too Many Requests when exceeded

### JWT Authentication
- Protected routes require `Authorization: Bearer <token>` header
- Public routes: `/health`, `/ready`, `/metrics`, `/api/v1/auth/*`
- Returns 401 Unauthorized for missing/invalid tokens

### Order State Machine
- Valid state transitions:
  - `pending` → `confirmed` (via saga)
  - `confirmed` → `packing`
  - `packing` → `out_for_delivery`
  - `out_for_delivery` → `delivered`
  - `confirmed` → `cancelled` (triggers stock release)
- Invalid transitions return 400 Bad Request
- State changes publish events to RabbitMQ

### Notification Types
- `order_confirmed` - Order successfully placed
- `order_packing` - Order being prepared
- `order_out_for_delivery` - Order dispatched
- `order_delivered` - Order completed
- `order_cancelled` - Order cancelled

### Saga Compensation
- Multi-item order with partial failure triggers rollback
- Stock reservations for successful items are released
- Order fails atomically (no partial orders)

---

## Integration Test Suite

### Test Script (`scripts/test-all.sh`)
Comprehensive integration test with 61 test cases covering:

### Health & Readiness (10 tests)
- Health check for all 5 services
- Ready check for all 5 services (including Redis for gateway)

### Prometheus Metrics (9 tests)
- HTTP metrics (`http_requests_total`) on all services
- Business metrics: `users_registered_total`, `orders_created_total`, `inventory_level`, `notifications_sent_total`

### Authentication (9 tests)
- User registration returns 201 with JWT token and user ID
- Login returns 200
- Duplicate registration returns 409
- Bad login returns 401
- Login token works on protected routes
- Protected routes return 401 without token
- Protected routes return 200 with valid token

### User Profile (3 tests)
- Get user profile returns 200
- User profile excludes `password_hash` field
- Nonexistent user returns 404

### Product CRUD (3 tests)
- Create product returns 201 with ID
- Get product returns 200

### Order Saga (3 tests)
- Place order returns 201
- Order status is `confirmed`
- Stock decremented correctly

### Order Retrieval (4 tests)
- Get order by ID returns 200
- Order contains items
- List user orders returns 200
- User has orders

### Order State Machine (5 tests)
- `confirmed` → `packing` returns 200
- `packing` → `out_for_delivery` returns 200
- `out_for_delivery` → `delivered` returns 200
- Invalid transition (`delivered` → `pending`) returns 400

### Cancellation & Stock Release (2 tests)
- Cancel order returns 200
- Stock restored after cancellation

### Saga Compensation (2 tests)
- Order with insufficient stock fails (not 200/201)
- First item reservation rolled back (stock unchanged)

### Notifications (6 tests)
- Notifications exist for user
- Notification types exist: `order_confirmed`, `order_packing`, `order_out_for_delivery`, `order_delivered`, `order_cancelled`

### Structured Logging (1 test)
- Gateway logs are valid JSON

### Correlation ID (4 tests)
- Correlation ID echoed in response header
- Correlation ID found in 3+ service logs
- Auto-generated correlation ID when none sent
- Auto-generated correlation ID is valid UUID

### Rate Limiting (1 test)
- Parallel requests trigger 429 response

### Running Tests
```bash
make test-all
```
