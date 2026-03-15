# FreshCart - Online Grocery Delivery Platform

A microservices-based grocery delivery platform built with Go.

## Architecture

```
Client → API Gateway (8000) → User Service (8081) [REST]
                            → Product Service (8082) [REST]
                            → Order Service (8083) [REST]

Order Service → RabbitMQ (5672) → Notification Service (8084)
                                → Product Service (inventory updates)
```

## Services & Ports

| Service              | Port  | Database      | Description                          |
|---------------------|-------|---------------|--------------------------------------|
| API Gateway         | 8000  | Redis         | Reverse proxy, rate limiting, auth   |
| User Service        | 8081  | PostgreSQL    | Registration, login, JWT             |
| Product Service     | 8082  | PostgreSQL    | Catalog CRUD, inventory management   |
| Order Service       | 8083  | PostgreSQL    | Order placement, saga orchestration  |
| Notification Service| 8084  | MongoDB       | Event-driven notifications           |
| Redis               | 6379  | -             | Rate limiting storage                |
| RabbitMQ            | 5672  | -             | Message broker                       |
| RabbitMQ Management | 15672 | -             | Web UI (guest/guest)                 |

## Quick Start

```bash
# Build and start all services
make boot

# Seed test data (user + products)
make seed

# Check health of all services
make health

# View logs
make logs

# Stop all services
make down

# Clean up (remove volumes)
make clean
```

## API Endpoints

### User Service (via Gateway)
- `POST /api/v1/auth/register` - Register new user
- `POST /api/v1/auth/login` - Login, get JWT token
- `GET /api/v1/users/:id` - Get user profile

### Product Service (via Gateway)
- `GET /api/v1/products` - List all products
- `GET /api/v1/products/:id` - Get product details
- `POST /api/v1/products` - Create product
- `PATCH /api/v1/products/:id/stock` - Update stock

### Order Service (via Gateway)
- `POST /api/v1/orders` - Place order (runs saga)
- `GET /api/v1/orders/:id` - Get order details
- `GET /api/v1/orders/user/:user_id` - List user's orders

### Notification Service (direct)
- `GET /api/v1/notifications/user/:user_id` - List notifications

## Order Saga Flow

1. **Create Order** (status: PENDING)
2. **Reserve Inventory** - Decrement stock via Product Service
3. **Simulate Payment** - 100ms delay (always succeeds)
4. **Confirm Order** (status: CONFIRMED)
5. **Publish Event** - `order.confirmed` to RabbitMQ
6. **Notification** - Consumed, stored in MongoDB

If any step fails, compensation runs (release reserved stock).

## Testing End-to-End

```bash
# 1. Start everything
make boot

# 2. Wait for services (~20 seconds)
sleep 20

# 3. Seed data
make seed
# Note the USER_ID and PRODUCT_IDs from output

# 4. Place an order
curl -s -X POST http://localhost:8000/api/v1/orders \
  -H "Content-Type: application/json" \
  -d '{"user_id":"<USER_ID>","delivery_address":"123 Test St","items":[{"product_id":"<PRODUCT_ID>","quantity":2}]}' | jq .

# 5. Verify stock decremented
curl -s http://localhost:8000/api/v1/products/<PRODUCT_ID> | jq '.stock'

# 6. Check notification arrived (wait 2-3 seconds)
curl -s http://localhost:8084/api/v1/notifications/user/<USER_ID> | jq .

# 7. View RabbitMQ UI
open http://localhost:15672  # guest/guest
```

## Project Structure

```
freshcart/
├── services/
│   ├── api-gateway/          # Reverse proxy
│   ├── user-service/         # Auth + user management
│   ├── product-service/      # Product catalog + inventory
│   ├── order-service/        # Order placement + saga
│   └── notification-service/ # Event-driven notifications
├── scripts/
│   └── seed.sh               # Seed test data
├── docker-compose.yml
├── Makefile
└── README.md
```
