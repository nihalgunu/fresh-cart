package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var mongoClient *mongo.Client
var notificationsCollection *mongo.Collection

type Notification struct {
	ID        string    `json:"id" bson:"_id,omitempty"`
	OrderID   string    `json:"order_id" bson:"order_id"`
	UserID    string    `json:"user_id" bson:"user_id"`
	Type      string    `json:"type" bson:"type"`
	Status    string    `json:"status" bson:"status"`
	Message   string    `json:"message" bson:"message"`
	CreatedAt time.Time `json:"created_at" bson:"created_at"`
}

type OrderEvent struct {
	ID              string  `json:"id"`
	UserID          string  `json:"user_id"`
	Status          string  `json:"status"`
	TotalAmount     float64 `json:"total_amount"`
	DeliveryAddress string  `json:"delivery_address"`
}

func main() {
	connectMongoDB()
	go startRabbitMQConsumer()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/health", healthHandler)
	r.Get("/ready", readyHandler)
	r.Get("/api/v1/notifications/user/{user_id}", listUserNotificationsHandler)

	port := getEnv("PORT", "8084")
	log.Printf("Notification service starting on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}

func connectMongoDB() {
	mongoURI := getEnv("MONGODB_URI", "mongodb://localhost:27017")
	dbName := getEnv("MONGODB_DATABASE", "freshcart_notifications")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := 0; i < 30; i++ {
		var err error
		mongoClient, err = mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
		if err == nil {
			if err = mongoClient.Ping(ctx, nil); err == nil {
				break
			}
		}
		log.Printf("Waiting for MongoDB... (%d/30): %v", i+1, err)
		time.Sleep(time.Second)
	}

	notificationsCollection = mongoClient.Database(dbName).Collection("notifications")
	log.Println("MongoDB connected")
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

		// Declare exchange
		err = ch.ExchangeDeclare("orders", "topic", true, false, false, false, nil)
		if err != nil {
			log.Printf("Exchange declare failed: %v", err)
			ch.Close()
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		// Declare queue
		q, err := ch.QueueDeclare("notifications.order", true, false, false, false, nil)
		if err != nil {
			log.Printf("Queue declare failed: %v", err)
			ch.Close()
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		// Bind queue to exchange
		err = ch.QueueBind(q.Name, "order.confirmed", "orders", false, nil)
		if err != nil {
			log.Printf("Queue bind failed: %v", err)
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

		log.Println("RabbitMQ consumer started, listening for order.confirmed events")

		for msg := range msgs {
			var order OrderEvent
			if err := json.Unmarshal(msg.Body, &order); err != nil {
				log.Printf("Invalid message: %v", err)
				continue
			}

			log.Printf("Sending confirmation email to user %s for order %s", order.UserID, order.ID)

			notification := Notification{
				OrderID:   order.ID,
				UserID:    order.UserID,
				Type:      "order_confirmed",
				Status:    "sent",
				Message:   "Your order " + order.ID + " has been confirmed!",
				CreatedAt: time.Now(),
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := notificationsCollection.InsertOne(ctx, notification)
			cancel()
			if err != nil {
				log.Printf("Failed to store notification: %v", err)
			} else {
				log.Printf("Notification stored for order %s", order.ID)
			}
		}

		log.Println("RabbitMQ connection lost, reconnecting...")
		ch.Close()
		conn.Close()
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "notification-service"})
}

func readyHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := mongoClient.Ping(ctx, nil); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "not ready"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ready", "service": "notification-service"})
}

func listUserNotificationsHandler(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "user_id")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cursor, err := notificationsCollection.Find(ctx, bson.M{"user_id": userID}, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer cursor.Close(ctx)

	var notifications []Notification
	if err := cursor.All(ctx, &notifications); err != nil {
		http.Error(w, `{"error":"failed to decode"}`, http.StatusInternalServerError)
		return
	}

	if notifications == nil {
		notifications = []Notification{}
	}

	json.NewEncoder(w).Encode(notifications)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
