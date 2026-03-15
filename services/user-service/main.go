// Package main implements the User Service for the FreshCart e-commerce platform.
//
// The User Service handles all user-related operations including:
//   - User registration with bcrypt password hashing
//   - User authentication with JWT token generation (24-hour expiry)
//   - User profile management and retrieval
//   - Correlation ID tracking for distributed tracing
//   - Prometheus metrics for user registration tracking
//
// Database: PostgreSQL (freshcart_users)
//
// Routes:
//
//	POST   /api/v1/auth/register    Register a new user
//	POST   /api/v1/auth/login       Authenticate and get JWT token
//	GET    /api/v1/users/{id}       Get user profile by ID
//	GET    /health                  Liveness probe
//	GET    /ready                   Readiness probe
//	GET    /metrics                 Prometheus metrics
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

	"user-service/internal/tracing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/crypto/bcrypt"
)

// serviceName is the identifier used for logging, metrics, and tracing.
const serviceName = "user-service"

// contextKey is a custom type for context keys to avoid collisions.
type contextKey string

// correlationIDKey stores the unique request identifier for distributed tracing.
const correlationIDKey contextKey = "correlation_id"

// Global dependencies
var (
	// db is the PostgreSQL database connection pool.
	db *sql.DB
	// jwtSecret is the signing key for JWT tokens.
	jwtSecret []byte
)

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

	usersRegisteredTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "users_registered_total",
			Help: "Total number of users registered",
		},
	)
)

func init() {
	prometheus.MustRegister(httpRequestsTotal)
	prometheus.MustRegister(httpRequestDuration)
	prometheus.MustRegister(usersRegisteredTotal)
}

// User represents a registered user in the system.
// Password hash is excluded from JSON serialization for security.
type User struct {
	ID              string    `json:"id"`
	Email           string    `json:"email"`
	Name            string    `json:"name"`
	DeliveryAddress string    `json:"delivery_address"`
	CreatedAt       time.Time `json:"created_at"`
}

// RegisterRequest contains the data required to create a new user account.
type RegisterRequest struct {
	Email           string `json:"email"`
	Password        string `json:"password"`
	Name            string `json:"name"`
	DeliveryAddress string `json:"delivery_address"`
}

// LoginRequest contains credentials for user authentication.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// AuthResponse is returned after successful registration or login.
// Contains user details and a JWT token for subsequent authenticated requests.
type AuthResponse struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Token string `json:"token"`
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

	jwtSecret = []byte(getEnv("JWT_SECRET", "freshcart-dev-secret"))

	var dbErr error
	db, dbErr = sql.Open("postgres", getEnv("DATABASE_URL", "postgres://freshcart:freshcart@localhost:5432/freshcart_users?sslmode=disable"))
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

	r := chi.NewRouter()
	r.Use(correlationIDMiddleware)
	r.Use(requestLoggingMiddleware)
	r.Use(metricsMiddleware)
	r.Use(middleware.Recoverer)

	// Metrics endpoint
	r.Handle("/metrics", promhttp.Handler())

	r.Get("/health", healthHandler)
	r.Get("/ready", readyHandler)

	r.Post("/api/v1/auth/register", registerHandler)
	r.Post("/api/v1/auth/login", loginHandler)
	r.Get("/api/v1/users/{id}", getUserHandler)

	port := getEnv("PORT", "8081")
	slog.Info("user-service starting", "service", serviceName, "port", port)

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

// migrate creates the users table if it doesn't exist.
// Uses PostgreSQL's pgcrypto extension for UUID generation.
func migrate() {
	schema := `
	CREATE EXTENSION IF NOT EXISTS "pgcrypto";
	CREATE TABLE IF NOT EXISTS users (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		email VARCHAR(255) UNIQUE NOT NULL,
		password_hash VARCHAR(255) NOT NULL,
		name VARCHAR(255) NOT NULL,
		delivery_address TEXT DEFAULT '',
		created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
		updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
	);`
	if _, err := db.Exec(schema); err != nil {
		slog.Warn("migration warning", "service", serviceName, "error", err.Error())
	}
	slog.Info("migration completed", "service", serviceName)
}

// healthHandler returns a simple health check response for liveness probes.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": serviceName})
}

// readyHandler checks database connectivity for readiness probes.
// Returns HTTP 503 if the database is unreachable.
func readyHandler(w http.ResponseWriter, r *http.Request) {
	if err := db.Ping(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "not ready"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ready", "service": serviceName})
}

// registerHandler creates a new user account.
// It hashes the password using bcrypt, stores the user in the database,
// and returns a JWT token for immediate authentication.
// Returns HTTP 409 Conflict if the email already exists.
func registerHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	correlationID := getCorrelationID(ctx)

	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	var id string
	err = db.QueryRow(
		`INSERT INTO users (email, password_hash, name, delivery_address) VALUES ($1, $2, $3, $4) RETURNING id`,
		req.Email, string(hash), req.Name, req.DeliveryAddress,
	).Scan(&id)
	if err != nil {
		slog.Warn("registration failed",
			"service", serviceName,
			"correlation_id", correlationID,
			"email", req.Email,
			"reason", "email already exists",
		)
		http.Error(w, `{"error":"email already exists"}`, http.StatusConflict)
		return
	}

	token := generateToken(id, req.Email)

	// Increment users registered counter
	usersRegisteredTotal.Inc()

	slog.Info("user registered",
		"service", serviceName,
		"correlation_id", correlationID,
		"user_id", id,
		"email", req.Email,
	)

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(AuthResponse{ID: id, Email: req.Email, Name: req.Name, Token: token})
}

// loginHandler authenticates a user and returns a JWT token.
// It verifies the password against the stored bcrypt hash and generates
// a new JWT token valid for 24 hours.
// Returns HTTP 401 Unauthorized for invalid credentials.
func loginHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	correlationID := getCorrelationID(ctx)

	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	var id, name, passwordHash string
	err := db.QueryRow(`SELECT id, name, password_hash FROM users WHERE email = $1`, req.Email).Scan(&id, &name, &passwordHash)
	if err != nil {
		slog.Warn("login failed",
			"service", serviceName,
			"correlation_id", correlationID,
			"email", req.Email,
			"reason", "user not found",
		)
		http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)); err != nil {
		slog.Warn("login failed",
			"service", serviceName,
			"correlation_id", correlationID,
			"email", req.Email,
			"reason", "invalid credentials",
		)
		http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	token := generateToken(id, req.Email)

	slog.Info("user logged in",
		"service", serviceName,
		"correlation_id", correlationID,
		"user_id", id,
		"email", req.Email,
	)

	json.NewEncoder(w).Encode(AuthResponse{ID: id, Email: req.Email, Name: name, Token: token})
}

// getUserHandler retrieves a user profile by ID.
// Returns HTTP 400 for invalid UUIDs, HTTP 404 if user not found.
func getUserHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	var user User
	err := db.QueryRow(`SELECT id, email, name, delivery_address, created_at FROM users WHERE id = $1`, id).
		Scan(&user.ID, &user.Email, &user.Name, &user.DeliveryAddress, &user.CreatedAt)
	if err != nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(user)
}

// generateToken creates a signed JWT token for the given user.
// The token contains the user ID (sub), email, and expires after 24 hours.
func generateToken(userID, email string) string {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":   userID,
		"email": email,
		"exp":   time.Now().Add(24 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
	})
	signed, _ := token.SignedString(jwtSecret)
	return signed
}

// getEnv retrieves an environment variable value or returns the fallback if not set.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
