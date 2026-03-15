package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
var rabbitConn *amqp.Connection
var rabbitCh *amqp.Channel
var productServiceURL string

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

func main() {
	productServiceURL = getEnv("PRODUCT_SERVICE_URL", "http://localhost:8082")

	var err error
	db, err = sql.Open("postgres", getEnv("DATABASE_URL", "postgres://freshcart:freshcart@localhost:5432/freshcart_orders?sslmode=disable"))
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
	connectRabbitMQ()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/health", healthHandler)
	r.Get("/ready", readyHandler)

	r.Post("/api/v1/orders", createOrderHandler)
	r.Get("/api/v1/orders/{id}", getOrderHandler)
	r.Get("/api/v1/orders/user/{user_id}", listUserOrdersHandler)

	port := getEnv("PORT", "8083")
	log.Printf("Order service starting on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
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
		log.Printf("Migration warning: %v", err)
	}
	log.Println("Database migrated")
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
					log.Println("RabbitMQ connected")
					return
				}
			}
		}
		log.Printf("Waiting for RabbitMQ... (%d/30): %v", i+1, err)
		time.Sleep(time.Second)
	}
	log.Println("Warning: RabbitMQ not connected, events won't be published")
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "order-service"})
}

func readyHandler(w http.ResponseWriter, r *http.Request) {
	if err := db.Ping(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "not ready"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ready", "service": "order-service"})
}

func createOrderHandler(w http.ResponseWriter, r *http.Request) {
	var req CreateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	// SAGA Step 1: Get product info and reserve inventory
	var items []OrderItem
	var totalAmount float64
	var reservedProducts []string // Track for compensation

	for _, item := range req.Items {
		// Get product info
		product, err := getProduct(item.ProductID)
		if err != nil {
			releaseReservedStock(reservedProducts, req.Items)
			http.Error(w, fmt.Sprintf(`{"error":"product not found: %s"}`, item.ProductID), http.StatusBadRequest)
			return
		}

		// Reserve stock (decrement)
		if err := updateStock(item.ProductID, -item.Quantity); err != nil {
			releaseReservedStock(reservedProducts, req.Items)
			http.Error(w, fmt.Sprintf(`{"error":"insufficient stock for product: %s"}`, product.Name), http.StatusConflict)
			return
		}

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

	// SAGA Step 3: Create order in database
	tx, err := db.Begin()
	if err != nil {
		releaseReservedStock(reservedProducts, req.Items)
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
		releaseReservedStock(reservedProducts, req.Items)
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
			releaseReservedStock(reservedProducts, req.Items)
			http.Error(w, `{"error":"failed to create order items"}`, http.StatusInternalServerError)
			return
		}
		items[i].ID = itemID
	}

	if err := tx.Commit(); err != nil {
		releaseReservedStock(reservedProducts, req.Items)
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

	publishOrderConfirmed(order)

	log.Printf("Order created: %s, total: %.2f", orderID, totalAmount)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(order)
}

func getProduct(productID string) (*Product, error) {
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/products/%s", productServiceURL, productID))
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

func updateStock(productID string, quantity int) error {
	body, _ := json.Marshal(map[string]int{"quantity": quantity})
	req, _ := http.NewRequest("PATCH", fmt.Sprintf("%s/api/v1/products/%s/stock", productServiceURL, productID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

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

func releaseReservedStock(productIDs []string, items []OrderItemRequest) {
	for i, pid := range productIDs {
		if i < len(items) {
			// Release by adding back the quantity
			if err := updateStock(pid, items[i].Quantity); err != nil {
				log.Printf("Failed to release stock for %s: %v", pid, err)
			}
		}
	}
}

func publishOrderConfirmed(order Order) {
	if rabbitCh == nil {
		log.Println("RabbitMQ not connected, skipping event publish")
		return
	}

	body, _ := json.Marshal(order)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := rabbitCh.PublishWithContext(ctx, "orders", "order.confirmed", false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        body,
	})
	if err != nil {
		log.Printf("Failed to publish order event: %v", err)
	} else {
		log.Printf("Published order.confirmed event for order %s", order.ID)
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

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
