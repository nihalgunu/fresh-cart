package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const serviceName = "notification-service"

type contextKey string

const correlationIDKey contextKey = "correlation_id"

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
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	connectMongoDB()
	go startRabbitMQConsumer()

	r := chi.NewRouter()
	r.Use(correlationIDMiddleware)
	r.Use(requestLoggingMiddleware)
	r.Use(middleware.Recoverer)

	r.Get("/health", healthHandler)
	r.Get("/ready", readyHandler)
	r.Get("/api/v1/notifications/user/{user_id}", listUserNotificationsHandler)

	port := getEnv("PORT", "8084")
	slog.Info("notification-service starting", "service", serviceName, "port", port)
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
		slog.Info("waiting for mongodb", "service", serviceName, "attempt", i+1, "max_attempts", 30, "error", err.Error())
		time.Sleep(time.Second)
	}

	notificationsCollection = mongoClient.Database(dbName).Collection("notifications")
	slog.Info("mongodb connected", "service", serviceName)
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

		// Declare exchange
		err = ch.ExchangeDeclare("orders", "topic", true, false, false, false, nil)
		if err != nil {
			slog.Warn("exchange declare failed",
				"service", serviceName,
				"error", err.Error(),
			)
			ch.Close()
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		// Declare queue
		q, err := ch.QueueDeclare("notifications.order", true, false, false, false, nil)
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

		// Bind queue to exchange
		err = ch.QueueBind(q.Name, "order.confirmed", "orders", false, nil)
		if err != nil {
			slog.Warn("queue bind failed",
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

			var order OrderEvent
			if err := json.Unmarshal(msg.Body, &order); err != nil {
				slog.Warn("invalid message",
					"service", serviceName,
					"correlation_id", correlationID,
					"error", err.Error(),
				)
				continue
			}

			slog.Info("order event received",
				"service", serviceName,
				"correlation_id", correlationID,
				"order_id", order.ID,
				"user_id", order.UserID,
			)

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
				slog.Error("notification store failed",
					"service", serviceName,
					"correlation_id", correlationID,
					"order_id", order.ID,
					"error", err.Error(),
				)
			} else {
				slog.Info("notification stored",
					"service", serviceName,
					"correlation_id", correlationID,
					"order_id", order.ID,
					"user_id", order.UserID,
					"type", "order_confirmed",
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := mongoClient.Ping(ctx, nil); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "not ready"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ready", "service": serviceName})
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
