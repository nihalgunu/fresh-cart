package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"notification-service/internal/tracing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const serviceName = "notification-service"

type contextKey string

const correlationIDKey contextKey = "correlation_id"

var mongoClient *mongo.Client
var notificationsCollection *mongo.Collection

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

	notificationsSentTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "notifications_sent_total",
			Help: "Total number of notifications sent",
		},
		[]string{"type"},
	)
)

func init() {
	prometheus.MustRegister(httpRequestsTotal)
	prometheus.MustRegister(httpRequestDuration)
	prometheus.MustRegister(notificationsSentTotal)
}

type Notification struct {
	ID        string    `json:"id" bson:"_id,omitempty"`
	OrderID   string    `json:"order_id" bson:"order_id"`
	UserID    string    `json:"user_id" bson:"user_id"`
	Type      string    `json:"type" bson:"type"`
	Status    string    `json:"status" bson:"status"`
	Message   string    `json:"message" bson:"message"`
	CreatedAt time.Time `json:"created_at" bson:"created_at"`
}

// OrderEvent for order creation (order.confirmed from saga)
type OrderEvent struct {
	ID              string  `json:"id"`
	UserID          string  `json:"user_id"`
	Status          string  `json:"status"`
	TotalAmount     float64 `json:"total_amount"`
	DeliveryAddress string  `json:"delivery_address"`
}

// OrderStatusEvent for status transitions
type OrderStatusEvent struct {
	OrderID        string `json:"order_id"`
	UserID         string `json:"user_id"`
	Status         string `json:"status"`
	PreviousStatus string `json:"previous_status"`
	UpdatedAt      string `json:"updated_at"`
}

var notificationMessages = map[string]string{
	"order.confirmed":        "Your order %s has been confirmed!",
	"order.packing":          "Your order %s is being packed!",
	"order.out_for_delivery": "Your order %s is out for delivery!",
	"order.delivered":        "Your order %s has been delivered!",
	"order.cancelled":        "Your order %s has been cancelled.",
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

	connectMongoDB()
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
	r.Get("/api/v1/notifications/user/{user_id}", listUserNotificationsHandler)

	port := getEnv("PORT", "8084")
	slog.Info("notification-service starting", "service", serviceName, "port", port)

	// Wrap handler with OpenTelemetry instrumentation
	handler := otelhttp.NewHandler(r, "server",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	if err := http.ListenAndServe(":"+port, handler); err != nil {
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
	queueName := "notifications.order"
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

		// Bind queue to exchange for all order status events
		routingKeys := []string{"order.confirmed", "order.packing", "order.out_for_delivery", "order.delivered", "order.cancelled"}
		bindFailed := false
		for _, key := range routingKeys {
			err = ch.QueueBind(q.Name, key, "orders", false, nil)
			if err != nil {
				slog.Warn("queue bind failed",
					"service", serviceName,
					"routing_key", key,
					"error", err.Error(),
				)
				bindFailed = true
				break
			}
		}
		if bindFailed {
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

			routingKey := msg.RoutingKey
			var orderID, userID string

			// Try parsing as OrderStatusEvent first (status transitions)
			var statusEvent OrderStatusEvent
			if err := json.Unmarshal(msg.Body, &statusEvent); err == nil && statusEvent.OrderID != "" {
				orderID = statusEvent.OrderID
				userID = statusEvent.UserID
			} else {
				// Fall back to OrderEvent (from saga - order creation)
				var orderEvent OrderEvent
				if err := json.Unmarshal(msg.Body, &orderEvent); err != nil {
					slog.Warn("message sent to DLQ",
						"service", serviceName,
						"msg", "message sent to DLQ",
						"queue", dlqName,
						"reason", "JSON parse error: "+err.Error(),
						"correlation_id", correlationID,
						"trace_id", traceID,
						"routing_key", routingKey,
					)
					span.End()
					// Nack without requeue - sends to DLQ
					msg.Nack(false, false)
					continue
				}
				orderID = orderEvent.ID
				userID = orderEvent.UserID
			}

			slog.Info("order event received",
				"service", serviceName,
				"correlation_id", correlationID,
				"trace_id", traceID,
				"routing_key", routingKey,
				"order_id", orderID,
				"user_id", userID,
			)

			// Get message template for this event type
			messageTemplate, ok := notificationMessages[routingKey]
			if !ok {
				slog.Warn("message sent to DLQ",
					"service", serviceName,
					"msg", "message sent to DLQ",
					"queue", dlqName,
					"reason", "unknown routing key: "+routingKey,
					"correlation_id", correlationID,
					"trace_id", traceID,
				)
				span.End()
				// Nack without requeue - sends to DLQ
				msg.Nack(false, false)
				continue
			}

			// Convert routing key to notification type (e.g., "order.confirmed" -> "order_confirmed")
			notificationType := routingKey
			if len(routingKey) > 6 && routingKey[:6] == "order." {
				notificationType = "order_" + routingKey[6:]
			}

			notification := Notification{
				OrderID:   orderID,
				UserID:    userID,
				Type:      notificationType,
				Status:    "sent",
				Message:   fmt.Sprintf(messageTemplate, orderID),
				CreatedAt: time.Now(),
			}

			dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := notificationsCollection.InsertOne(dbCtx, notification)
			cancel()
			if err != nil {
				slog.Error("message sent to DLQ",
					"service", serviceName,
					"msg", "message sent to DLQ",
					"queue", dlqName,
					"reason", "MongoDB error: "+err.Error(),
					"correlation_id", correlationID,
					"trace_id", traceID,
					"order_id", orderID,
				)
				span.End()
				// Nack without requeue - sends to DLQ
				msg.Nack(false, false)
				continue
			}

			// Increment notifications sent counter
			notificationsSentTotal.WithLabelValues(notificationType).Inc()

			slog.Info("notification stored",
				"service", serviceName,
				"correlation_id", correlationID,
				"trace_id", traceID,
				"order_id", orderID,
				"user_id", userID,
				"type", notificationType,
			)

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
