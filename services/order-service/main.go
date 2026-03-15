package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	amqp "github.com/rabbitmq/amqp091-go"
)

const serviceName = "order-service"

type contextKey string

const correlationIDKey contextKey = "correlation_id"

var db *sql.DB
var rabbitConn *amqp.Connection
var rabbitCh *amqp.Channel
var productServiceURL string

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

type Order struct {
	ID              string      `json:"id"`
	UserID          string      `json:"user_id"`
	Status          string      `json:"status"`
	TotalAmount     float64     `json:"total_amount"`
	DeliveryAddress string      `json:"delivery_address"`
	Items           []OrderItem `json:"items"`
	CreatedAt       time.Time   `json:"created_at"`
}

type OrderItem struct {
	ID          string  `json:"id"`
	ProductID   string  `json:"product_id"`
	ProductName string  `json:"product_name"`
	Quantity    int     `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
}

type CreateOrderRequest struct {
	UserID          string             `json:"user_id"`
	DeliveryAddress string             `json:"delivery_address"`
	Items           []OrderItemRequest `json:"items"`
}

type OrderItemRequest struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
}

type Product struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Price float64 `json:"price"`
	Stock int     `json:"stock"`
}

type StatusUpdateRequest struct {
	Status string `json:"status"`
}

type OrderStatusEvent struct {
	OrderID        string    `json:"order_id"`
	UserID         string    `json:"user_id"`
	Status         string    `json:"status"`
	PreviousStatus string    `json:"previous_status"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// validTransitions defines the state machine for order status
var validTransitions = map[string][]string{
	"pending":          {"confirmed", "failed"},
	"confirmed":        {"packing", "cancelled"},
	"packing":          {"out_for_delivery"},
	"out_for_delivery": {"delivered"},
	// delivered and failed are terminal states - no transitions allowed
}

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

	productServiceURL = getEnv("PRODUCT_SERVICE_URL", "http://localhost:8082")

	var err error
	db, err = sql.Open("postgres", getEnv("DATABASE_URL", "postgres://freshcart:freshcart@localhost:5432/freshcart_orders?sslmode=disable"))
	if err != nil {
		slog.Error("database connection failed", "service", serviceName, "error", err.Error())
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
	if err := http.ListenAndServe(":"+port, r); err != nil {
		slog.Error("server failed", "service", serviceName, "error", err.Error())
		os.Exit(1)
	}
}

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

func getCorrelationID(ctx context.Context) string {
	if id, ok := ctx.Value(correlationIDKey).(string); ok {
		return id
	}
	return ""
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func requestLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		duration := time.Since(start)
		slog.Info("request completed",
			"service", serviceName,
			"correlation_id", getCorrelationID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration_ms", duration.Milliseconds(),
		)
	})
}

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
	if _, err := db.Exec(schema); err != nil {
		slog.Warn("migration warning", "service", serviceName, "error", err.Error())
	}
	slog.Info("migration completed", "service", serviceName)
}

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

func healthHandler(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": serviceName})
}

func readyHandler(w http.ResponseWriter, r *http.Request) {
	if err := db.Ping(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "not ready"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ready", "service": serviceName})
}

func createOrderHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	correlationID := getCorrelationID(ctx)
	sagaStart := time.Now()

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

func getProduct(ctx context.Context, productID string) (*Product, error) {
	correlationID := getCorrelationID(ctx)

	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/v1/products/%s", productServiceURL, productID), nil)
	req.Header.Set("X-Correlation-ID", correlationID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
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

func updateStock(ctx context.Context, productID string, quantity int) error {
	correlationID := getCorrelationID(ctx)

	body, _ := json.Marshal(map[string]int{"quantity": quantity})
	req, _ := http.NewRequestWithContext(ctx, "PATCH", fmt.Sprintf("%s/api/v1/products/%s/stock", productServiceURL, productID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Correlation-ID", correlationID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
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

	body, _ := json.Marshal(order)
	pubCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := rabbitCh.PublishWithContext(pubCtx, "orders", "order.confirmed", false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        body,
		Headers: amqp.Table{
			"X-Correlation-ID": correlationID,
		},
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

	body, _ := json.Marshal(event)
	pubCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	routingKey := "order." + event.Status

	err := rabbitCh.PublishWithContext(pubCtx, "orders", routingKey, false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        body,
		Headers: amqp.Table{
			"X-Correlation-ID": correlationID,
		},
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

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
