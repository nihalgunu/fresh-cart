package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	amqp "github.com/rabbitmq/amqp091-go"
)

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
	var err error
	db, err = sql.Open("postgres", getEnv("DATABASE_URL", "postgres://freshcart:freshcart@localhost:5432/freshcart_products?sslmode=disable"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	for i := 0; i < 30; i++ {
		if err := db.Ping(); err == nil {
			break
		}
		log.Printf("Waiting for database... (%d/30)", i+1)
		time.Sleep(time.Second)
	}

	migrate()

	// Start RabbitMQ consumer in background
	go startRabbitMQConsumer()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/health", healthHandler)
	r.Get("/ready", readyHandler)

	r.Get("/api/v1/products", listProductsHandler)
	r.Get("/api/v1/products/{id}", getProductHandler)
	r.Post("/api/v1/products", createProductHandler)
	r.Patch("/api/v1/products/{id}/stock", updateStockHandler)

	port := getEnv("PORT", "8082")
	log.Printf("Product service starting on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
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
		log.Printf("Migration warning: %v", err)
	}
	log.Println("Database migrated")
}

func startRabbitMQConsumer() {
	rabbitURL := getEnv("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/")

	for {
		conn, err := amqp.Dial(rabbitURL)
		if err != nil {
			log.Printf("RabbitMQ connection failed, retrying in 5s: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		ch, err := conn.Channel()
		if err != nil {
			log.Printf("RabbitMQ channel failed: %v", err)
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		q, err := ch.QueueDeclare("inventory.update", true, false, false, false, nil)
		if err != nil {
			log.Printf("Queue declare failed: %v", err)
			ch.Close()
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		msgs, err := ch.Consume(q.Name, "", true, false, false, false, nil)
		if err != nil {
			log.Printf("Consume failed: %v", err)
			ch.Close()
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		log.Println("RabbitMQ consumer started")
		for msg := range msgs {
			var update struct {
				ProductID      string `json:"product_id"`
				QuantityChange int    `json:"quantity_change"`
				OrderID        string `json:"order_id"`
				Action         string `json:"action"`
			}
			if err := json.Unmarshal(msg.Body, &update); err != nil {
				log.Printf("Invalid message: %v", err)
				continue
			}
			log.Printf("Inventory update: product=%s, change=%d, action=%s", update.ProductID, update.QuantityChange, update.Action)
			_, err := db.Exec(`UPDATE products SET stock = stock + $1, updated_at = NOW() WHERE id = $2`, update.QuantityChange, update.ProductID)
			if err != nil {
				log.Printf("Stock update failed: %v", err)
			}
		}

		log.Println("RabbitMQ connection lost, reconnecting...")
		ch.Close()
		conn.Close()
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "product-service"})
}

func readyHandler(w http.ResponseWriter, r *http.Request) {
	if err := db.Ping(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "not ready"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ready", "service": "product-service"})
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
		log.Printf("Create product error: %v", err)
		http.Error(w, `{"error":"failed to create product"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(p)
}

func updateStockHandler(w http.ResponseWriter, r *http.Request) {
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

	json.NewEncoder(w).Encode(p)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
