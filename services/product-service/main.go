package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"product-service/internal/tracing"

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

const serviceName = "product-service"

type contextKey string

const correlationIDKey contextKey = "correlation_id"

var db *sql.DB

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

	inventoryLevel = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "inventory_level",
			Help: "Current inventory level for products",
		},
		[]string{"product_id"},
	)

	inventoryUpdatesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "inventory_updates_total",
			Help: "Total number of inventory updates",
		},
		[]string{"product_id", "action"},
	)
)

func init() {
	prometheus.MustRegister(httpRequestsTotal)
	prometheus.MustRegister(httpRequestDuration)
	prometheus.MustRegister(inventoryLevel)
	prometheus.MustRegister(inventoryUpdatesTotal)
}

type Product struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Price       float64   `json:"price"`
	Category    string    `json:"category"`
	Stock       int       `json:"stock"`
	ImageURL    string    `json:"image_url"`
	CreatedAt   time.Time `json:"created_at"`
}

type CreateProductRequest struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Price       float64 `json:"price"`
	Category    string  `json:"category"`
	Stock       int     `json:"stock"`
	ImageURL    string  `json:"image_url"`
}

type StockUpdateRequest struct {
	Quantity int `json:"quantity"`
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

	var dbErr error
	db, dbErr = sql.Open("postgres", getEnv("DATABASE_URL", "postgres://freshcart:freshcart@localhost:5432/freshcart_products?sslmode=disable"))
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

	// Initialize inventory metrics from existing products
	initInventoryMetrics()

	// Start RabbitMQ consumer in background
	go startRabbitMQConsumer()

	r := chi.NewRouter()
	r.Use(correlationIDMiddleware)
	r.Use(requestLoggingMiddleware)
	r.Use(metricsMiddleware)
	r.Use(middleware.Recoverer)

	// Metrics endpoint
	r.Handle("/metrics", promhttp.Handler())

	r.Get("/health", healthHandler)
	r.Get("/ready", readyHandler)

	r.Get("/api/v1/products", listProductsHandler)
	r.Get("/api/v1/products/search", searchProductsHandler)
	r.Get("/api/v1/products/{id}", getProductHandler)
	r.Post("/api/v1/products", createProductHandler)
	r.Patch("/api/v1/products/{id}/stock", updateStockHandler)

	port := getEnv("PORT", "8082")
	slog.Info("product-service starting", "service", serviceName, "port", port)

	// Wrap handler with OpenTelemetry instrumentation
	handler := otelhttp.NewHandler(r, "server",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		slog.Error("server failed", "service", serviceName, "error", err.Error())
		os.Exit(1)
	}
}

func initInventoryMetrics() {
	rows, err := db.Query(`SELECT id, stock FROM products`)
	if err != nil {
		slog.Warn("failed to initialize inventory metrics", "service", serviceName, "error", err.Error())
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var stock int
		if err := rows.Scan(&id, &stock); err != nil {
			continue
		}
		inventoryLevel.WithLabelValues(id).Set(float64(stock))
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
	CREATE TABLE IF NOT EXISTS products (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name VARCHAR(255) NOT NULL,
		description TEXT DEFAULT '',
		price DECIMAL(10,2) NOT NULL,
		category VARCHAR(100) NOT NULL,
		stock INTEGER NOT NULL DEFAULT 0,
		image_url TEXT DEFAULT '',
		created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
		updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
	);`
	if _, err := db.Exec(schema); err != nil {
		slog.Warn("migration warning", "service", serviceName, "error", err.Error())
	}
	slog.Info("migration completed", "service", serviceName)
}

func startRabbitMQConsumer() {
	rabbitURL := getEnv("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/")
	queueName := "inventory.update"
	dlqName := queueName + ".dlq"

	for {
		conn, err := amqp.Dial(rabbitURL)
		if err != nil {
			slog.Warn("rabbitmq connection failed, retrying",
				"service", serviceName,
				"retry_in", "5s",
				"error", err.Error(),
			)
			time.Sleep(5 * time.Second)
			continue
		}

		ch, err := conn.Channel()
		if err != nil {
			slog.Warn("rabbitmq channel failed",
				"service", serviceName,
				"error", err.Error(),
			)
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		// Declare the Dead Letter Queue first (simple queue, no special args)
		_, err = ch.QueueDeclare(dlqName, true, false, false, false, nil)
		if err != nil {
			slog.Warn("dlq declare failed",
				"service", serviceName,
				"queue", dlqName,
				"error", err.Error(),
			)
			ch.Close()
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		// Declare main queue with DLQ arguments
		args := amqp.Table{
			"x-dead-letter-exchange":    "",
			"x-dead-letter-routing-key": dlqName,
		}
		q, err := ch.QueueDeclare(queueName, true, false, false, false, args)
		if err != nil {
			slog.Warn("queue declare failed",
				"service", serviceName,
				"error", err.Error(),
			)
			ch.Close()
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		// Manual ack mode (autoAck = false)
		msgs, err := ch.Consume(q.Name, "", false, false, false, false, nil)
		if err != nil {
			slog.Warn("consume failed",
				"service", serviceName,
				"error", err.Error(),
			)
			ch.Close()
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		slog.Info("rabbitmq consumer started", "service", serviceName, "queue", queueName, "dlq", dlqName)

		tracer := otel.Tracer(serviceName)

		for msg := range msgs {
			// Extract correlation ID from AMQP headers
			correlationID := ""
			if msg.Headers != nil {
				if cid, ok := msg.Headers["X-Correlation-ID"].(string); ok {
					correlationID = cid
				}
			}

			// Extract OTel context from AMQP headers
			carrier := propagation.MapCarrier{}
			for k, v := range msg.Headers {
				if s, ok := v.(string); ok {
					carrier[k] = s
				}
			}
			ctx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)

			// Create a span for processing this message
			ctx, span := tracer.Start(ctx, "rabbitmq.consume",
				trace.WithSpanKind(trace.SpanKindConsumer),
			)

			// Get trace ID for logging
			traceID := ""
			spanCtx := trace.SpanFromContext(ctx).SpanContext()
			if spanCtx.HasTraceID() {
				traceID = spanCtx.TraceID().String()
			}

			var update struct {
				ProductID      string `json:"product_id"`
				QuantityChange int    `json:"quantity_change"`
				OrderID        string `json:"order_id"`
				Action         string `json:"action"`
			}
			if err := json.Unmarshal(msg.Body, &update); err != nil {
				slog.Warn("message sent to DLQ",
					"service", serviceName,
					"msg", "message sent to DLQ",
					"queue", dlqName,
					"reason", "JSON parse error: "+err.Error(),
					"correlation_id", correlationID,
					"trace_id", traceID,
				)
				span.End()
				// Nack without requeue - sends to DLQ
				msg.Nack(false, false)
				continue
			}

			slog.Info("inventory update received",
				"service", serviceName,
				"correlation_id", correlationID,
				"trace_id", traceID,
				"product_id", update.ProductID,
				"action", update.Action,
				"order_id", update.OrderID,
				"quantity_change", update.QuantityChange,
			)

			result, err := db.Exec(`UPDATE products SET stock = stock + $1, updated_at = NOW() WHERE id = $2`, update.QuantityChange, update.ProductID)
			if err != nil {
				slog.Error("message sent to DLQ",
					"service", serviceName,
					"msg", "message sent to DLQ",
					"queue", dlqName,
					"reason", "DB error: "+err.Error(),
					"correlation_id", correlationID,
					"trace_id", traceID,
					"product_id", update.ProductID,
				)
				span.End()
				// Nack without requeue - sends to DLQ
				msg.Nack(false, false)
				continue
			}

			if rowsAffected, _ := result.RowsAffected(); rowsAffected > 0 {
				// Update inventory gauge
				var newStock int
				if err := db.QueryRow(`SELECT stock FROM products WHERE id = $1`, update.ProductID).Scan(&newStock); err == nil {
					inventoryLevel.WithLabelValues(update.ProductID).Set(float64(newStock))
				}
				inventoryUpdatesTotal.WithLabelValues(update.ProductID, update.Action).Inc()
			}

			span.End()
			// Successfully processed - acknowledge the message
			msg.Ack(false)
		}

		slog.Warn("rabbitmq connection lost, reconnecting", "service", serviceName)
		ch.Close()
		conn.Close()
	}
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

func listProductsHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT id, name, description, price, category, stock, image_url, created_at FROM products ORDER BY created_at DESC`)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	products := []Product{}
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.Price, &p.Category, &p.Stock, &p.ImageURL, &p.CreatedAt); err != nil {
			continue
		}
		products = append(products, p)
	}

	json.NewEncoder(w).Encode(products)
}

func searchProductsHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	correlationID := getCorrelationID(ctx)

	// Parse query parameters
	q := r.URL.Query().Get("q")
	category := r.URL.Query().Get("category")
	minPriceStr := r.URL.Query().Get("min_price")
	maxPriceStr := r.URL.Query().Get("max_price")
	inStockStr := r.URL.Query().Get("in_stock")

	// Build dynamic query
	query := `SELECT id, name, description, price, category, stock, image_url, created_at FROM products WHERE 1=1`
	args := []interface{}{}
	argNum := 1

	if q != "" {
		query += ` AND (name ILIKE '%' || $` + strconv.Itoa(argNum) + ` || '%' OR description ILIKE '%' || $` + strconv.Itoa(argNum) + ` || '%')`
		args = append(args, q)
		argNum++
	}

	if category != "" {
		query += ` AND category = $` + strconv.Itoa(argNum)
		args = append(args, category)
		argNum++
	}

	if minPriceStr != "" {
		minPrice, err := strconv.ParseFloat(minPriceStr, 64)
		if err == nil {
			query += ` AND price >= $` + strconv.Itoa(argNum)
			args = append(args, minPrice)
			argNum++
		}
	}

	if maxPriceStr != "" {
		maxPrice, err := strconv.ParseFloat(maxPriceStr, 64)
		if err == nil {
			query += ` AND price <= $` + strconv.Itoa(argNum)
			args = append(args, maxPrice)
			argNum++
		}
	}

	if inStockStr == "true" {
		query += ` AND stock > 0`
	}

	query += ` ORDER BY name`

	rows, err := db.Query(query, args...)
	if err != nil {
		slog.Error("product search failed",
			"service", serviceName,
			"correlation_id", correlationID,
			"error", err.Error(),
		)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	products := []Product{}
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.Price, &p.Category, &p.Stock, &p.ImageURL, &p.CreatedAt); err != nil {
			continue
		}
		products = append(products, p)
	}

	slog.Info("product search",
		"service", serviceName,
		"correlation_id", correlationID,
		"query", q,
		"category", category,
		"results", len(products),
	)

	json.NewEncoder(w).Encode(products)
}

func getProductHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	var p Product
	err := db.QueryRow(`SELECT id, name, description, price, category, stock, image_url, created_at FROM products WHERE id = $1`, id).
		Scan(&p.ID, &p.Name, &p.Description, &p.Price, &p.Category, &p.Stock, &p.ImageURL, &p.CreatedAt)
	if err != nil {
		http.Error(w, `{"error":"product not found"}`, http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(p)
}

func createProductHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	correlationID := getCorrelationID(ctx)

	var req CreateProductRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	var p Product
	err := db.QueryRow(
		`INSERT INTO products (name, description, price, category, stock, image_url) VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, name, description, price, category, stock, image_url, created_at`,
		req.Name, req.Description, req.Price, req.Category, req.Stock, req.ImageURL,
	).Scan(&p.ID, &p.Name, &p.Description, &p.Price, &p.Category, &p.Stock, &p.ImageURL, &p.CreatedAt)
	if err != nil {
		slog.Error("create product failed",
			"service", serviceName,
			"correlation_id", correlationID,
			"error", err.Error(),
		)
		http.Error(w, `{"error":"failed to create product"}`, http.StatusInternalServerError)
		return
	}

	// Update inventory metrics
	inventoryLevel.WithLabelValues(p.ID).Set(float64(p.Stock))
	inventoryUpdatesTotal.WithLabelValues(p.ID, "create").Inc()

	slog.Info("product created",
		"service", serviceName,
		"correlation_id", correlationID,
		"product_id", p.ID,
		"name", p.Name,
	)

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(p)
}

func updateStockHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	correlationID := getCorrelationID(ctx)

	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	var req StockUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	// Check if stock would go negative
	var currentStock int
	err := db.QueryRow(`SELECT stock FROM products WHERE id = $1`, id).Scan(&currentStock)
	if err != nil {
		http.Error(w, `{"error":"product not found"}`, http.StatusNotFound)
		return
	}

	if currentStock+req.Quantity < 0 {
		slog.Warn("insufficient stock",
			"service", serviceName,
			"correlation_id", correlationID,
			"product_id", id,
			"requested", -req.Quantity,
			"available", currentStock,
		)
		http.Error(w, `{"error":"insufficient stock"}`, http.StatusConflict)
		return
	}

	var p Product
	err = db.QueryRow(
		`UPDATE products SET stock = stock + $1, updated_at = NOW() WHERE id = $2
		 RETURNING id, name, description, price, category, stock, image_url, created_at`,
		req.Quantity, id,
	).Scan(&p.ID, &p.Name, &p.Description, &p.Price, &p.Category, &p.Stock, &p.ImageURL, &p.CreatedAt)
	if err != nil {
		http.Error(w, `{"error":"failed to update stock"}`, http.StatusInternalServerError)
		return
	}

	// Update inventory metrics
	inventoryLevel.WithLabelValues(id).Set(float64(p.Stock))
	action := "manual_update"
	if req.Quantity < 0 {
		action = "reserve"
	} else {
		action = "release"
	}
	inventoryUpdatesTotal.WithLabelValues(id, action).Inc()

	slog.Info("stock updated",
		"service", serviceName,
		"correlation_id", correlationID,
		"product_id", id,
		"old_stock", currentStock,
		"new_stock", p.Stock,
		"change", req.Quantity,
	)

	json.NewEncoder(w).Encode(p)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
