package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	amqp "github.com/rabbitmq/amqp091-go"
)

const serviceName = "product-service"

type contextKey string

const correlationIDKey contextKey = "correlation_id"

var db *sql.DB

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

	var err error
	db, err = sql.Open("postgres", getEnv("DATABASE_URL", "postgres://freshcart:freshcart@localhost:5432/freshcart_products?sslmode=disable"))
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

	// Start RabbitMQ consumer in background
	go startRabbitMQConsumer()

	r := chi.NewRouter()
	r.Use(correlationIDMiddleware)
	r.Use(requestLoggingMiddleware)
	r.Use(middleware.Recoverer)

	r.Get("/health", healthHandler)
	r.Get("/ready", readyHandler)

	r.Get("/api/v1/products", listProductsHandler)
	r.Get("/api/v1/products/{id}", getProductHandler)
	r.Post("/api/v1/products", createProductHandler)
	r.Patch("/api/v1/products/{id}/stock", updateStockHandler)

	port := getEnv("PORT", "8082")
	slog.Info("product-service starting", "service", serviceName, "port", port)
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

		q, err := ch.QueueDeclare("inventory.update", true, false, false, false, nil)
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

		msgs, err := ch.Consume(q.Name, "", true, false, false, false, nil)
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

		slog.Info("rabbitmq consumer started", "service", serviceName)

		for msg := range msgs {
			// Extract correlation ID from AMQP headers
			correlationID := ""
			if msg.Headers != nil {
				if cid, ok := msg.Headers["X-Correlation-ID"].(string); ok {
					correlationID = cid
				}
			}

			var update struct {
				ProductID      string `json:"product_id"`
				QuantityChange int    `json:"quantity_change"`
				OrderID        string `json:"order_id"`
				Action         string `json:"action"`
			}
			if err := json.Unmarshal(msg.Body, &update); err != nil {
				slog.Warn("invalid message",
					"service", serviceName,
					"correlation_id", correlationID,
					"error", err.Error(),
				)
				continue
			}

			slog.Info("inventory update received",
				"service", serviceName,
				"correlation_id", correlationID,
				"product_id", update.ProductID,
				"action", update.Action,
				"order_id", update.OrderID,
				"quantity_change", update.QuantityChange,
			)

			_, err := db.Exec(`UPDATE products SET stock = stock + $1, updated_at = NOW() WHERE id = $2`, update.QuantityChange, update.ProductID)
			if err != nil {
				slog.Error("stock update failed",
					"service", serviceName,
					"correlation_id", correlationID,
					"product_id", update.ProductID,
					"error", err.Error(),
				)
			}
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
