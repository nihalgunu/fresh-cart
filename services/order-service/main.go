// Package main implements the Order Service for the FreshCart e-commerce platform.
//
// The Order Service is the core orchestrator for order processing, implementing:
//   - Saga pattern for distributed transaction coordination
//   - Order lifecycle state machine (pending → confirmed → packing → out_for_delivery → delivered)
//   - Inventory reservation with compensating transactions
//   - Circuit breaker and retry patterns for resilient inter-service communication
//   - Bulkhead pattern limiting concurrent saga executions
//   - RabbitMQ event publishing for downstream services
//   - Prometheus metrics for order tracking and saga duration
//
// Database: PostgreSQL (freshcart_orders)
//
// Saga Flow (createOrderHandler):
//
//	1. Validate products exist (GET product-service)
//	2. Reserve inventory (PATCH product-service/stock)
//	3. Simulate payment (100ms delay)
//	4. Create order in database (transaction)
//	5. Publish order.confirmed event
//	6. On any failure: execute compensating transactions to release stock
//
// State Machine Transitions:
//
//	pending → confirmed, failed
//	confirmed → packing, cancelled
//	packing → out_for_delivery
//	out_for_delivery → delivered
//	(delivered and failed are terminal states)
//
// Routes:
//
//	POST   /api/v1/orders                Create a new order (triggers saga)
//	GET    /api/v1/orders/{id}           Get order details with items
//	GET    /api/v1/orders/user/{user_id} List orders for a user
//	PATCH  /api/v1/orders/{id}/status    Update order status
//	GET    /health                       Liveness probe
//	GET    /ready                        Readiness probe
//	GET    /metrics                      Prometheus metrics
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"order-service/internal/resilience"
	"order-service/internal/tracing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// serviceName is the identifier used for logging, metrics, and tracing.
const serviceName = "order-service"

// contextKey is a custom type for context keys to avoid collisions.
type contextKey string

// correlationIDKey stores the unique request identifier for distributed tracing.
const correlationIDKey contextKey = "correlation_id"

// Global dependencies
var (
	// db is the PostgreSQL database connection pool.
	db *sql.DB
	// rabbitConn is the RabbitMQ connection.
	rabbitConn *amqp.Connection
	// rabbitCh is the RabbitMQ channel for publishing events.
	rabbitCh *amqp.Channel
	// productServiceURL is the base URL for the product service.
	productServiceURL string
	// productServiceClient is a resilient HTTP client with circuit breaker and retry.
	productServiceClient *resilience.ResilientClient
)

// maxConcurrentSagas limits concurrent saga executions (bulkhead pattern).
const maxConcurrentSagas = 10

// sagaSemaphore implements the bulkhead pattern by limiting concurrent saga executions.
var sagaSemaphore = make(chan struct{}, maxConcurrentSagas)

var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "path", "status"},
	)

	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)

	ordersCreatedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orders_created_total",
			Help: "Total number of orders created",
		},
		[]string{"status"},
	)

	orderSagaDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "order_saga_duration_seconds",
			Help:    "Order saga duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
	)
)

func init() {
	prometheus.MustRegister(httpRequestsTotal)
	prometheus.MustRegister(httpRequestDuration)
	prometheus.MustRegister(ordersCreatedTotal)
	prometheus.MustRegister(orderSagaDuration)
}

// Order represents a customer order with its items.
type Order struct {
	ID              string      `json:"id"`
	UserID          string      `json:"user_id"`
	Status          string      `json:"status"`
	TotalAmount     float64     `json:"total_amount"`
	DeliveryAddress string      `json:"delivery_address"`
	Items           []OrderItem `json:"items"`
	CreatedAt       time.Time   `json:"created_at"`
}

// OrderItem represents a line item within an order.
type OrderItem struct {
	ID          string  `json:"id"`
	ProductID   string  `json:"product_id"`
	ProductName string  `json:"product_name"`
	Quantity    int     `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
}

// CreateOrderRequest contains the data required to create a new order.
type CreateOrderRequest struct {
	UserID          string             `json:"user_id"`
	DeliveryAddress string             `json:"delivery_address"`
	Items           []OrderItemRequest `json:"items"`
}

// OrderItemRequest represents a product and quantity to add to an order.
type OrderItemRequest struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
}

// Product represents product data fetched from the product service.
type Product struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Price float64 `json:"price"`
	Stock int     `json:"stock"`
}

// StatusUpdateRequest contains the new status for an order transition.
type StatusUpdateRequest struct {
	Status string `json:"status"`
}

// OrderStatusEvent is published to RabbitMQ when an order status changes.
type OrderStatusEvent struct {
	OrderID        string    `json:"order_id"`
	UserID         string    `json:"user_id"`
	Status         string    `json:"status"`
	PreviousStatus string    `json:"previous_status"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// validTransitions defines the state machine for order status.
// Each key maps to a slice of valid target states.
// Terminal states (delivered, failed) have no outgoing transitions.
var validTransitions = map[string][]string{
	"pending":          {"confirmed", "failed"},
	"confirmed":        {"packing", "cancelled"},
	"packing":          {"out_for_delivery"},
	"out_for_delivery": {"delivered"},
	// delivered and failed are terminal states - no transitions allowed
}

// isValidTransition checks if a status transition is valid according to the state machine.
func isValidTransition(from, to string) bool {
	allowedStates, exists := validTransitions[from]
	if !exists {
		return false
	}
	for _, state := range allowedStates {
		if state == to {
			return true
		}
	}
	return false
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// Initialize OpenTelemetry tracer
	shutdown, err := tracing.InitTracer(serviceName)
	if err != nil {
		slog.Error("failed to init tracer", "service", serviceName, "error", err.Error())
	} else {
		defer shutdown(context.Background())
		slog.Info("tracer initialized", "service", serviceName)
	}

	productServiceURL = getEnv("PRODUCT_SERVICE_URL", "http://localhost:8082")

	// Initialize resilient HTTP client for product service
	productServiceClient = resilience.NewResilientClient(
		"product-service",
		5*time.Second, // 5 second timeout
		resilience.DefaultConfig(),
		resilience.DefaultRetryConfig(),
	)

	var dbErr error
	db, dbErr = sql.Open("postgres", getEnv("DATABASE_URL", "postgres://freshcart:freshcart@localhost:5432/freshcart_orders?sslmode=disable"))
	if dbErr != nil {
		slog.Error("database connection failed", "service", serviceName, "error", dbErr.Error())
		os.Exit(1)
	}
	defer db.Close()

	for i := 0; i < 30; i++ {
		if err := db.Ping(); err == nil {
			break
		}
		slog.Info("waiting for database", "service", serviceName, "attempt", i+1, "max_attempts", 30)
		time.Sleep(time.Second)
	}

	migrate()
	connectRabbitMQ()

	r := chi.NewRouter()
	r.Use(correlationIDMiddleware)
	r.Use(requestLoggingMiddleware)
	r.Use(metricsMiddleware)
	r.Use(middleware.Recoverer)

	// Metrics endpoint
	r.Handle("/metrics", promhttp.Handler())

	r.Get("/health", healthHandler)
	r.Get("/ready", readyHandler)

	r.Post("/api/v1/orders", createOrderHandler)
	r.Get("/api/v1/orders/{id}", getOrderHandler)
	r.Get("/api/v1/orders/user/{user_id}", listUserOrdersHandler)
	r.Patch("/api/v1/orders/{id}/status", updateOrderStatusHandler)

	port := getEnv("PORT", "8083")
	slog.Info("order-service starting", "service", serviceName, "port", port)

	// Wrap handler with OpenTelemetry instrumentation
	handler := otelhttp.NewHandler(r, "server",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		slog.Error("server failed", "service", serviceName, "error", err.Error())
		os.Exit(1)
	}
}

// correlationIDMiddleware ensures every request has a correlation ID for distributed tracing.
func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlationID := r.Header.Get("X-Correlation-ID")
		if correlationID == "" {
			correlationID = uuid.New().String()
		}

		ctx := context.WithValue(r.Context(), correlationIDKey, correlationID)
		w.Header().Set("X-Correlation-ID", correlationID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// getCorrelationID retrieves the correlation ID from the context.
func getCorrelationID(ctx context.Context) string {
	if id, ok := ctx.Value(correlationIDKey).(string); ok {
		return id
	}
	return ""
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

// WriteHeader captures the status code and delegates to the underlying ResponseWriter.
func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// requestLoggingMiddleware logs each HTTP request with timing and trace information.
func requestLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		duration := time.Since(start)

		// Extract trace ID from span context
		logAttrs := []any{
			"service", serviceName,
			"correlation_id", getCorrelationID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration_ms", duration.Milliseconds(),
		}

		spanCtx := trace.SpanFromContext(r.Context()).SpanContext()
		if spanCtx.HasTraceID() {
			logAttrs = append(logAttrs, "trace_id", spanCtx.TraceID().String())
		}

		slog.Info("request completed", logAttrs...)
	})
}

// metricsMiddleware collects Prometheus metrics for each HTTP request.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		duration := time.Since(start).Seconds()
		httpRequestsTotal.WithLabelValues(r.Method, r.URL.Path, strconv.Itoa(wrapped.statusCode)).Inc()
		httpRequestDuration.WithLabelValues(r.Method, r.URL.Path).Observe(duration)
	})
}

// migrate creates the orders and order_items tables if they don't exist.
// Retries up to 30 times with 1 second delay to handle K8s DNS propagation delays.
func migrate() {
	schema := `
	CREATE EXTENSION IF NOT EXISTS "pgcrypto";
	CREATE TABLE IF NOT EXISTS orders (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		user_id UUID NOT NULL,
		status VARCHAR(50) NOT NULL DEFAULT 'pending',
		total_amount DECIMAL(10,2) NOT NULL DEFAULT 0,
		delivery_address TEXT NOT NULL,
		created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
		updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
	);
	CREATE TABLE IF NOT EXISTS order_items (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		order_id UUID REFERENCES orders(id),
		product_id UUID NOT NULL,
		product_name VARCHAR(255) NOT NULL,
		quantity INTEGER NOT NULL,
		unit_price DECIMAL(10,2) NOT NULL
	);`
	for i := 0; i < 30; i++ {
		if _, err := db.Exec(schema); err != nil {
			slog.Warn("migration attempt failed, retrying", "service", serviceName, "attempt", i+1, "error", err.Error())
			time.Sleep(time.Second)
			continue
		}
		slog.Info("migration completed", "service", serviceName)
		return
	}
	slog.Error("migration failed after 30 attempts", "service", serviceName)
}

// connectRabbitMQ establishes a connection to RabbitMQ with retry logic.
// Declares the "orders" topic exchange for publishing order events.
func connectRabbitMQ() {
	rabbitURL := getEnv("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/")

	for i := 0; i < 30; i++ {
		var err error
		rabbitConn, err = amqp.Dial(rabbitURL)
		if err == nil {
			rabbitCh, err = rabbitConn.Channel()
			if err == nil {
				// Declare exchange
				err = rabbitCh.ExchangeDeclare("orders", "topic", true, false, false, false, nil)
				if err == nil {
					slog.Info("rabbitmq connected", "service", serviceName)
					return
				}
			}
		}
		slog.Info("waiting for rabbitmq", "service", serviceName, "attempt", i+1, "max_attempts", 30, "error", err.Error())
		time.Sleep(time.Second)
	}
	slog.Warn("rabbitmq not connected, events won't be published", "service", serviceName)
}

// healthHandler returns a simple health check response for liveness probes.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": serviceName})
}

// readyHandler checks database connectivity for readiness probes.
func readyHandler(w http.ResponseWriter, r *http.Request) {
	if err := db.Ping(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "not ready"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ready", "service": serviceName})
}

// createOrderHandler implements the order creation saga.
// It orchestrates inventory reservation, payment simulation, order creation,
// and event publishing. On any failure, compensating transactions release reserved stock.
//
// The saga is protected by a bulkhead (semaphore) limiting concurrent executions
// and uses circuit breakers for resilient inter-service communication.
func createOrderHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	correlationID := getCorrelationID(ctx)
	sagaStart := time.Now()

	// Bulkhead: limit concurrent saga executions
	select {
	case sagaSemaphore <- struct{}{}:
		defer func() { <-sagaSemaphore }()
	case <-ctx.Done():
		slog.Warn("saga bulkhead full",
			"service", serviceName,
			"correlation_id", correlationID,
			"msg", "saga bulkhead full",
			"concurrent_sagas", maxConcurrentSagas,
		)
		http.Error(w, `{"error":"saga execution timed out waiting for capacity"}`, http.StatusServiceUnavailable)
		return
	}

	var req CreateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	slog.Info("saga started",
		"service", serviceName,
		"correlation_id", correlationID,
		"user_id", req.UserID,
		"item_count", len(req.Items),
	)

	// SAGA Step 1: Get product info and reserve inventory
	var items []OrderItem
	var totalAmount float64
	var reservedProducts []string // Track for compensation

	for _, item := range req.Items {
		// Get product info
		product, err := getProduct(ctx, item.ProductID)
		if err != nil {
			slog.Warn("inventory reservation failed",
				"service", serviceName,
				"correlation_id", correlationID,
				"product_id", item.ProductID,
				"reason", "product not found",
			)
			releaseReservedStock(ctx, reservedProducts, req.Items)
			ordersCreatedTotal.WithLabelValues("failed").Inc()
			orderSagaDuration.Observe(time.Since(sagaStart).Seconds())
			http.Error(w, fmt.Sprintf(`{"error":"product not found: %s"}`, item.ProductID), http.StatusBadRequest)
			return
		}

		// Reserve stock (decrement)
		if err := updateStock(ctx, item.ProductID, -item.Quantity); err != nil {
			slog.Warn("inventory reservation failed",
				"service", serviceName,
				"correlation_id", correlationID,
				"product_id", item.ProductID,
				"reason", err.Error(),
			)
			releaseReservedStock(ctx, reservedProducts, req.Items)
			ordersCreatedTotal.WithLabelValues("failed").Inc()
			orderSagaDuration.Observe(time.Since(sagaStart).Seconds())
			http.Error(w, fmt.Sprintf(`{"error":"insufficient stock for product: %s"}`, product.Name), http.StatusConflict)
			return
		}

		slog.Info("inventory reserved",
			"service", serviceName,
			"correlation_id", correlationID,
			"product_id", item.ProductID,
			"quantity", item.Quantity,
		)

		reservedProducts = append(reservedProducts, item.ProductID)
		items = append(items, OrderItem{
			ProductID:   item.ProductID,
			ProductName: product.Name,
			Quantity:    item.Quantity,
			UnitPrice:   product.Price,
		})
		totalAmount += product.Price * float64(item.Quantity)
	}

	// SAGA Step 2: Simulate payment (100ms delay, always succeeds for now)
	time.Sleep(100 * time.Millisecond)
	slog.Info("payment simulated",
		"service", serviceName,
		"correlation_id", correlationID,
		"amount", totalAmount,
	)

	// SAGA Step 3: Create order in database
	tx, err := db.Begin()
	if err != nil {
		slog.Error("order failed",
			"service", serviceName,
			"correlation_id", correlationID,
			"reason", "database transaction failed",
		)
		releaseReservedStock(ctx, reservedProducts, req.Items)
		ordersCreatedTotal.WithLabelValues("failed").Inc()
		orderSagaDuration.Observe(time.Since(sagaStart).Seconds())
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	var orderID string
	err = tx.QueryRow(
		`INSERT INTO orders (user_id, status, total_amount, delivery_address) VALUES ($1, $2, $3, $4) RETURNING id`,
		req.UserID, "confirmed", totalAmount, req.DeliveryAddress,
	).Scan(&orderID)
	if err != nil {
		tx.Rollback()
		slog.Error("order failed",
			"service", serviceName,
			"correlation_id", correlationID,
			"reason", "failed to create order record",
		)
		releaseReservedStock(ctx, reservedProducts, req.Items)
		ordersCreatedTotal.WithLabelValues("failed").Inc()
		orderSagaDuration.Observe(time.Since(sagaStart).Seconds())
		http.Error(w, `{"error":"failed to create order"}`, http.StatusInternalServerError)
		return
	}

	for i := range items {
		var itemID string
		err := tx.QueryRow(
			`INSERT INTO order_items (order_id, product_id, product_name, quantity, unit_price) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			orderID, items[i].ProductID, items[i].ProductName, items[i].Quantity, items[i].UnitPrice,
		).Scan(&itemID)
		if err != nil {
			tx.Rollback()
			slog.Error("order failed",
				"service", serviceName,
				"correlation_id", correlationID,
				"order_id", orderID,
				"reason", "failed to create order items",
			)
			releaseReservedStock(ctx, reservedProducts, req.Items)
			ordersCreatedTotal.WithLabelValues("failed").Inc()
			orderSagaDuration.Observe(time.Since(sagaStart).Seconds())
			http.Error(w, `{"error":"failed to create order items"}`, http.StatusInternalServerError)
			return
		}
		items[i].ID = itemID
	}

	if err := tx.Commit(); err != nil {
		slog.Error("order failed",
			"service", serviceName,
			"correlation_id", correlationID,
			"order_id", orderID,
			"reason", "failed to commit transaction",
		)
		releaseReservedStock(ctx, reservedProducts, req.Items)
		ordersCreatedTotal.WithLabelValues("failed").Inc()
		orderSagaDuration.Observe(time.Since(sagaStart).Seconds())
		http.Error(w, `{"error":"failed to commit order"}`, http.StatusInternalServerError)
		return
	}

	// SAGA Step 4: Publish event to RabbitMQ
	order := Order{
		ID:              orderID,
		UserID:          req.UserID,
		Status:          "confirmed",
		TotalAmount:     totalAmount,
		DeliveryAddress: req.DeliveryAddress,
		Items:           items,
		CreatedAt:       time.Now(),
	}

	publishOrderConfirmed(ctx, order)

	// Record successful order metrics
	ordersCreatedTotal.WithLabelValues("confirmed").Inc()
	orderSagaDuration.Observe(time.Since(sagaStart).Seconds())

	slog.Info("order confirmed",
		"service", serviceName,
		"correlation_id", correlationID,
		"order_id", orderID,
		"total", totalAmount,
	)

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(order)
}

// getProduct fetches product details from the product service.
// Uses the resilient client with circuit breaker and retry patterns.
func getProduct(ctx context.Context, productID string) (*Product, error) {
	correlationID := getCorrelationID(ctx)

	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/v1/products/%s", productServiceURL, productID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Correlation-ID", correlationID)

	resp, err := productServiceClient.Do(ctx, req)
	if err != nil {
		if err == resilience.ErrCircuitOpen {
			slog.Warn("circuit breaker open",
				"service", serviceName,
				"correlation_id", correlationID,
				"target", "product-service",
				"path", req.URL.Path,
			)
		}
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("product not found")
	}

	var product Product
	if err := json.NewDecoder(resp.Body).Decode(&product); err != nil {
		return nil, err
	}
	return &product, nil
}

// updateStock adjusts product inventory via the product service.
// Uses the resilient client with circuit breaker and retry patterns.
// Positive quantity releases stock, negative quantity reserves stock.
func updateStock(ctx context.Context, productID string, quantity int) error {
	correlationID := getCorrelationID(ctx)

	body, _ := json.Marshal(map[string]int{"quantity": quantity})
	req, err := http.NewRequestWithContext(ctx, "PATCH", fmt.Sprintf("%s/api/v1/products/%s/stock", productServiceURL, productID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Correlation-ID", correlationID)
	// GetBody allows the request body to be re-read on retries
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	resp, err := productServiceClient.Do(ctx, req)
	if err != nil {
		if err == resilience.ErrCircuitOpen {
			slog.Warn("circuit breaker open",
				"service", serviceName,
				"correlation_id", correlationID,
				"target", "product-service",
				"path", req.URL.Path,
			)
		}
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("insufficient stock")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stock update failed")
	}
	return nil
}

// releaseReservedStock is a compensating transaction that releases previously reserved stock.
// Called when the saga fails after inventory reservation to maintain consistency.
func releaseReservedStock(ctx context.Context, productIDs []string, items []OrderItemRequest) {
	correlationID := getCorrelationID(ctx)

	for i, pid := range productIDs {
		if i < len(items) {
			slog.Info("compensating transaction",
				"service", serviceName,
				"correlation_id", correlationID,
				"product_id", pid,
				"action", "releasing reserved stock",
			)
			// Release by adding back the quantity
			if err := updateStock(ctx, pid, items[i].Quantity); err != nil {
				slog.Error("compensation failed",
					"service", serviceName,
					"correlation_id", correlationID,
					"product_id", pid,
					"error", err.Error(),
				)
			}
		}
	}
}

// publishOrderConfirmed publishes an order.confirmed event to RabbitMQ.
// The event is consumed by the notification service to send confirmation messages.
// Includes OpenTelemetry trace context propagation for distributed tracing.
func publishOrderConfirmed(ctx context.Context, order Order) {
	correlationID := getCorrelationID(ctx)

	if rabbitCh == nil {
		slog.Warn("rabbitmq not connected, skipping event publish",
			"service", serviceName,
			"correlation_id", correlationID,
			"order_id", order.ID,
		)
		return
	}

	// Create a span for RabbitMQ publish
	tracer := otel.Tracer(serviceName)
	ctx, span := tracer.Start(ctx, "rabbitmq.publish",
		trace.WithSpanKind(trace.SpanKindProducer),
	)
	defer span.End()

	body, _ := json.Marshal(order)
	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Build headers with correlation ID and OTel context
	headers := amqp.Table{
		"X-Correlation-ID": correlationID,
	}

	// Inject OTel context into headers
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		headers[k] = v
	}

	err := rabbitCh.PublishWithContext(pubCtx, "orders", "order.confirmed", false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        body,
		Headers:     headers,
	})
	if err != nil {
		slog.Error("event publish failed",
			"service", serviceName,
			"correlation_id", correlationID,
			"order_id", order.ID,
			"error", err.Error(),
		)
	} else {
		slog.Info("event published",
			"service", serviceName,
			"correlation_id", correlationID,
			"exchange", "orders",
			"routing_key", "order.confirmed",
			"order_id", order.ID,
		)
	}
}

// getOrderHandler retrieves an order by ID including all order items.
// Returns HTTP 400 for invalid UUIDs, HTTP 404 if order not found.
func getOrderHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	var order Order
	err := db.QueryRow(
		`SELECT id, user_id, status, total_amount, delivery_address, created_at FROM orders WHERE id = $1`,
		id,
	).Scan(&order.ID, &order.UserID, &order.Status, &order.TotalAmount, &order.DeliveryAddress, &order.CreatedAt)
	if err != nil {
		http.Error(w, `{"error":"order not found"}`, http.StatusNotFound)
		return
	}

	rows, _ := db.Query(`SELECT id, product_id, product_name, quantity, unit_price FROM order_items WHERE order_id = $1`, id)
	defer rows.Close()
	for rows.Next() {
		var item OrderItem
		rows.Scan(&item.ID, &item.ProductID, &item.ProductName, &item.Quantity, &item.UnitPrice)
		order.Items = append(order.Items, item)
	}

	json.NewEncoder(w).Encode(order)
}

// listUserOrdersHandler returns all orders for a specific user, ordered by creation date (newest first).
// Returns HTTP 400 for invalid UUIDs.
func listUserOrdersHandler(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "user_id")
	if _, err := uuid.Parse(userID); err != nil {
		http.Error(w, `{"error":"invalid user_id"}`, http.StatusBadRequest)
		return
	}

	rows, err := db.Query(`SELECT id, user_id, status, total_amount, delivery_address, created_at FROM orders WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	orders := []Order{}
	for rows.Next() {
		var o Order
		rows.Scan(&o.ID, &o.UserID, &o.Status, &o.TotalAmount, &o.DeliveryAddress, &o.CreatedAt)
		orders = append(orders, o)
	}

	json.NewEncoder(w).Encode(orders)
}

// updateOrderStatusHandler transitions an order to a new status.
// Validates the transition against the state machine.
// For cancellation, releases reserved inventory before updating status.
// Publishes a status event to RabbitMQ for downstream services.
func updateOrderStatusHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	correlationID := getCorrelationID(ctx)

	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	var req StatusUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	// Get current order status
	var currentStatus string
	var userID string
	err := db.QueryRow(`SELECT status, user_id FROM orders WHERE id = $1`, id).Scan(&currentStatus, &userID)
	if err == sql.ErrNoRows {
		http.Error(w, `{"error":"order not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	// Validate transition
	if !isValidTransition(currentStatus, req.Status) {
		slog.Warn("invalid status transition",
			"service", serviceName,
			"correlation_id", correlationID,
			"order_id", id,
			"from", currentStatus,
			"to", req.Status,
		)
		http.Error(w, fmt.Sprintf(`{"error":"invalid status transition from %s to %s"}`, currentStatus, req.Status), http.StatusBadRequest)
		return
	}

	// Handle cancellation - release stock before updating status
	if req.Status == "cancelled" {
		if err := releaseStockForOrder(ctx, id); err != nil {
			slog.Error("failed to release stock for cancelled order",
				"service", serviceName,
				"correlation_id", correlationID,
				"order_id", id,
				"error", err.Error(),
			)
			http.Error(w, `{"error":"failed to release stock"}`, http.StatusInternalServerError)
			return
		}
	}

	// Update status in database
	_, err = db.Exec(`UPDATE orders SET status = $1, updated_at = NOW() WHERE id = $2`, req.Status, id)
	if err != nil {
		http.Error(w, `{"error":"failed to update status"}`, http.StatusInternalServerError)
		return
	}

	slog.Info("order status changed",
		"service", serviceName,
		"correlation_id", correlationID,
		"order_id", id,
		"from", currentStatus,
		"to", req.Status,
	)

	// Publish event
	event := OrderStatusEvent{
		OrderID:        id,
		UserID:         userID,
		Status:         req.Status,
		PreviousStatus: currentStatus,
		UpdatedAt:      time.Now(),
	}
	publishOrderEvent(ctx, event)

	// Return updated order with items
	var order Order
	err = db.QueryRow(
		`SELECT id, user_id, status, total_amount, delivery_address, created_at FROM orders WHERE id = $1`,
		id,
	).Scan(&order.ID, &order.UserID, &order.Status, &order.TotalAmount, &order.DeliveryAddress, &order.CreatedAt)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch updated order"}`, http.StatusInternalServerError)
		return
	}

	rows, _ := db.Query(`SELECT id, product_id, product_name, quantity, unit_price FROM order_items WHERE order_id = $1`, id)
	defer rows.Close()
	for rows.Next() {
		var item OrderItem
		rows.Scan(&item.ID, &item.ProductID, &item.ProductName, &item.Quantity, &item.UnitPrice)
		order.Items = append(order.Items, item)
	}

	json.NewEncoder(w).Encode(order)
}

// releaseStockForOrder releases all reserved stock for a cancelled order.
// Iterates through order items and releases inventory for each product.
func releaseStockForOrder(ctx context.Context, orderID string) error {
	correlationID := getCorrelationID(ctx)

	rows, err := db.Query(`SELECT product_id, quantity FROM order_items WHERE order_id = $1`, orderID)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var productID string
		var quantity int
		if err := rows.Scan(&productID, &quantity); err != nil {
			return err
		}

		slog.Info("releasing stock for cancelled order",
			"service", serviceName,
			"correlation_id", correlationID,
			"order_id", orderID,
			"product_id", productID,
			"quantity", quantity,
		)

		if err := updateStock(ctx, productID, quantity); err != nil {
			slog.Error("failed to release stock",
				"service", serviceName,
				"correlation_id", correlationID,
				"order_id", orderID,
				"product_id", productID,
				"error", err.Error(),
			)
			return err
		}
	}

	return nil
}

// publishOrderEvent publishes an order status change event to RabbitMQ.
// The routing key is based on the new status (e.g., "order.packing", "order.delivered").
// Includes OpenTelemetry trace context propagation for distributed tracing.
func publishOrderEvent(ctx context.Context, event OrderStatusEvent) {
	correlationID := getCorrelationID(ctx)

	if rabbitCh == nil {
		slog.Warn("rabbitmq not connected, skipping event publish",
			"service", serviceName,
			"correlation_id", correlationID,
			"order_id", event.OrderID,
		)
		return
	}

	// Create a span for RabbitMQ publish
	tracer := otel.Tracer(serviceName)
	ctx, span := tracer.Start(ctx, "rabbitmq.publish",
		trace.WithSpanKind(trace.SpanKindProducer),
	)
	defer span.End()

	body, _ := json.Marshal(event)
	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	routingKey := "order." + event.Status

	// Build headers with correlation ID and OTel context
	headers := amqp.Table{
		"X-Correlation-ID": correlationID,
	}

	// Inject OTel context into headers
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		headers[k] = v
	}

	err := rabbitCh.PublishWithContext(pubCtx, "orders", routingKey, false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        body,
		Headers:     headers,
	})
	if err != nil {
		slog.Error("event publish failed",
			"service", serviceName,
			"correlation_id", correlationID,
			"order_id", event.OrderID,
			"routing_key", routingKey,
			"error", err.Error(),
		)
	} else {
		slog.Info("event published",
			"service", serviceName,
			"correlation_id", correlationID,
			"exchange", "orders",
			"routing_key", routingKey,
			"order_id", event.OrderID,
		)
	}
}

// getEnv retrieves an environment variable value or returns the fallback if not set.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
