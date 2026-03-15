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
