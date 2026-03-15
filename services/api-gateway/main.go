// Package main implements the API Gateway service for the FreshCart e-commerce platform.
//
// The API Gateway serves as the single entry point for all client requests, providing:
//   - JWT-based authentication and authorization
//   - Redis-backed rate limiting using sliding window algorithm
//   - Reverse proxy routing to downstream microservices
//   - Correlation ID generation and propagation for distributed tracing
//   - OpenTelemetry instrumentation for observability
//   - Prometheus metrics collection
//
// Architecture:
//
//	Client → API Gateway → [user-service, product-service, order-service, notification-service]
//
// Routes:
//
//	POST   /api/v1/auth/register       (public)
//	POST   /api/v1/auth/login          (public)
//	GET    /api/v1/users/*             (protected)
//	GET    /api/v1/products/*          (protected)
//	GET    /api/v1/orders/*            (protected)
//	GET    /api/v1/notifications/*     (protected)
//	GET    /metrics                    (Prometheus endpoint)
//	GET    /health, /ready             (liveness/readiness probes)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"api-gateway/internal/tracing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// serviceName is the identifier used for logging, metrics, and tracing.
const serviceName = "api-gateway"

// contextKey is a custom type for context keys to avoid collisions.
type contextKey string

// Context keys for storing request-scoped values.
const (
	// correlationIDKey stores the unique request identifier for distributed tracing.
	correlationIDKey contextKey = "correlation_id"
	// userIDKey stores the authenticated user's ID extracted from JWT.
	userIDKey contextKey = "user_id"
	// userEmailKey stores the authenticated user's email extracted from JWT.
	userEmailKey contextKey = "user_email"
)

var (
	// redisClient is the global Redis client used for rate limiting.
	redisClient *redis.Client

	// httpRequestsTotal counts total HTTP requests by method, path, and status code.
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "path", "status"},
	)

	// httpRequestDuration measures HTTP request latency in seconds.
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)
)

func init() {
	prometheus.MustRegister(httpRequestsTotal)
	prometheus.MustRegister(httpRequestDuration)
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

	// Initialize Redis client
	redisURL := getEnv("REDIS_URL", "redis://localhost:6379")
	redisOpts, err := redis.ParseURL(redisURL)
	if err != nil {
		slog.Error("failed to parse redis URL", "service", serviceName, "error", err.Error())
		os.Exit(1)
	}
	redisClient = redis.NewClient(redisOpts)

	// Test Redis connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		slog.Warn("redis not available at startup", "service", serviceName, "error", err.Error())
	} else {
		slog.Info("redis connected", "service", serviceName)
	}

	// Get rate limit config
	rateLimitRPS, _ := strconv.Atoi(getEnv("RATE_LIMIT_RPS", "10"))
	rateLimitBurst, _ := strconv.Atoi(getEnv("RATE_LIMIT_BURST", "20"))

	r := chi.NewRouter()

	// Rate limiting is the OUTERMOST middleware (applied to ALL routes)
	r.Use(rateLimitMiddleware(rateLimitRPS, rateLimitBurst))
	r.Use(correlationIDMiddleware)
	r.Use(requestLoggingMiddleware)
	r.Use(metricsMiddleware)
	r.Use(middleware.Recoverer)

	// Metrics endpoint (public)
	r.Handle("/metrics", promhttp.Handler())

	// Health endpoints (public, no auth required)
	r.Get("/health", healthHandler)
	r.Get("/ready", readyHandler)

	// Service URLs
	userServiceURL := getEnv("USER_SERVICE_URL", "http://localhost:8081")
	productServiceURL := getEnv("PRODUCT_SERVICE_URL", "http://localhost:8082")
	orderServiceURL := getEnv("ORDER_SERVICE_URL", "http://localhost:8083")
	notificationServiceURL := getEnv("NOTIFICATION_SERVICE_URL", "http://localhost:8084")

	r.Route("/api/v1", func(r chi.Router) {
		// Public auth routes (no auth middleware)
		r.Mount("/auth", proxyHandler(userServiceURL, "/api/v1/auth", "user-service"))

		// Protected routes (with auth middleware)
		r.Group(func(r chi.Router) {
			r.Use(authMiddleware)
			r.Mount("/users", proxyHandler(userServiceURL, "/api/v1/users", "user-service"))
			r.Mount("/products", proxyHandler(productServiceURL, "/api/v1/products", "product-service"))
			r.Mount("/orders", proxyHandler(orderServiceURL, "/api/v1/orders", "order-service"))
			r.Mount("/notifications", proxyHandler(notificationServiceURL, "/api/v1/notifications", "notification-service"))
		})
	})

	port := getEnv("PORT", "8080")
	slog.Info("api-gateway starting", "service", serviceName, "port", port)

	// Wrap handler with OpenTelemetry instrumentation
	handler := otelhttp.NewHandler(r, "server",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		slog.Error("server failed", "service", serviceName, "error", err.Error())
		os.Exit(1)
	}
}

// healthHandler returns a simple health check response for liveness probes.
// It always returns HTTP 200 with {"status": "ok"} if the service is running.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": serviceName})
}

// readyHandler checks if the service and its dependencies are ready to accept traffic.
// It verifies Redis connectivity and returns HTTP 503 if Redis is unavailable.
// Used by Kubernetes readiness probes to determine if traffic should be routed to this instance.
func readyHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Check Redis connectivity
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	redisStatus := "ok"
	if err := redisClient.Ping(ctx).Err(); err != nil {
		redisStatus = "error: " + err.Error()
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "not ready",
			"service": serviceName,
			"checks":  map[string]string{"redis": redisStatus},
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ready",
		"service": serviceName,
		"checks":  map[string]string{"redis": redisStatus},
	})
}

// rateLimitMiddleware creates a rate limiting middleware using Redis-backed sliding window algorithm.
// It limits requests per client IP based on the configured requests per second (rps) and burst parameters.
// If Redis is unavailable, requests are allowed through (fail-open behavior).
//
// Parameters:
//   - rps: Maximum requests per second per client
//   - burst: Maximum burst size allowed within a 1-second window
//
// Returns HTTP 429 Too Many Requests when rate limit is exceeded.
func rateLimitMiddleware(rps, burst int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clientIP := getClientIP(r)
			correlationID := r.Header.Get("X-Correlation-ID")
			if correlationID == "" {
				correlationID = getCorrelationID(r.Context())
			}

			// Use sliding window rate limiting with Redis
			ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
			defer cancel()

			key := fmt.Sprintf("ratelimit:%s", clientIP)
			now := time.Now().Unix()
			windowStart := now - 1 // 1 second window

			// Remove old entries and count current window
			pipe := redisClient.Pipeline()
			pipe.ZRemRangeByScore(ctx, key, "0", strconv.FormatInt(windowStart, 10))
			countCmd := pipe.ZCard(ctx, key)
			_, err := pipe.Exec(ctx)

			if err != nil && err != redis.Nil {
				// If Redis is unavailable, allow the request (fail open)
				slog.Warn("rate limit check failed", "service", serviceName, "error", err.Error(), "ip", clientIP)
				next.ServeHTTP(w, r)
				return
			}

			count := countCmd.Val()

			if count >= int64(burst) {
				slog.Info("rate limited",
					"service", serviceName,
					"ip", clientIP,
					"correlation_id", correlationID,
				)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded"})
				return
			}

			// Add current request to the window
			redisClient.ZAdd(ctx, key, redis.Z{
				Score:  float64(now),
				Member: fmt.Sprintf("%d-%s", now, uuid.New().String()),
			})
			redisClient.Expire(ctx, key, 2*time.Second)

			next.ServeHTTP(w, r)
		})
	}
}

// getClientIP extracts the client IP address from the request.
// It first checks the X-Forwarded-For header (for requests behind a proxy/load balancer),
// then falls back to the RemoteAddr field. The port is stripped from the result.
func getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header first
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		// Take the first IP in the list
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}

	// Fall back to RemoteAddr
	ip := r.RemoteAddr
	// Remove port if present
	if colonIdx := strings.LastIndex(ip, ":"); colonIdx != -1 {
		ip = ip[:colonIdx]
	}
	return ip
}

// authMiddleware validates JWT tokens in the Authorization header and extracts user claims.
// It expects a Bearer token and validates the signature using HS256 algorithm.
// On successful validation, user ID and email are stored in the request context.
// Returns HTTP 401 Unauthorized for missing, invalid, or expired tokens.
func authMiddleware(next http.Handler) http.Handler {
	jwtSecret := []byte(getEnv("JWT_SECRET", "freshcart-dev-secret"))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlationID := getCorrelationID(r.Context())

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			slog.Info("auth failed",
				"service", serviceName,
				"reason", "missing authorization header",
				"path", r.URL.Path,
				"correlation_id", correlationID,
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "missing or invalid token"})
			return
		}

		// Extract Bearer token
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			slog.Info("auth failed",
				"service", serviceName,
				"reason", "invalid authorization format",
				"path", r.URL.Path,
				"correlation_id", correlationID,
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "missing or invalid token"})
			return
		}

		tokenString := parts[1]

		// Parse and validate the token
		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			// Validate the signing method
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			return jwtSecret, nil
		})

		if err != nil || !token.Valid {
			reason := "invalid token"
			if err != nil {
				reason = err.Error()
			}
			slog.Info("auth failed",
				"service", serviceName,
				"reason", reason,
				"path", r.URL.Path,
				"correlation_id", correlationID,
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "missing or invalid token"})
			return
		}

		// Extract claims
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			slog.Info("auth failed",
				"service", serviceName,
				"reason", "invalid claims",
				"path", r.URL.Path,
				"correlation_id", correlationID,
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "missing or invalid token"})
			return
		}

		// Extract user ID (sub) and email
		var userID string
		var email string

		if sub, ok := claims["sub"]; ok {
			switch v := sub.(type) {
			case float64:
				userID = strconv.FormatInt(int64(v), 10)
			case string:
				userID = v
			}
		}

		if e, ok := claims["email"].(string); ok {
			email = e
		}

		slog.Info("auth passed",
			"service", serviceName,
			"user_id", userID,
			"path", r.URL.Path,
			"correlation_id", correlationID,
		)

		// Store user info in context
		ctx := r.Context()
		ctx = context.WithValue(ctx, userIDKey, userID)
		ctx = context.WithValue(ctx, userEmailKey, email)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// correlationIDMiddleware ensures every request has a unique correlation ID for distributed tracing.
// If X-Correlation-ID header is present, it's used; otherwise, a new UUID is generated.
// The correlation ID is stored in the request context and returned in the response header.
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
// Returns an empty string if no correlation ID is found.
func getCorrelationID(ctx context.Context) string {
	if id, ok := ctx.Value(correlationIDKey).(string); ok {
		return id
	}
	return ""
}

// responseWriter wraps http.ResponseWriter to capture the status code for logging and metrics.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

// WriteHeader captures the status code and delegates to the underlying ResponseWriter.
func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// requestLoggingMiddleware logs each HTTP request with timing, status, and trace information.
// Logs are structured JSON format including correlation ID, trace ID, method, path, status, and duration.
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
// It records request count (by method, path, status) and request duration.
// The /metrics endpoint itself is excluded from metrics collection.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip metrics endpoint itself
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

// proxyHandler creates a reverse proxy handler for routing requests to a downstream service.
// It handles:
//   - URL path rewriting (strips and replaces prefix)
//   - Correlation ID forwarding
//   - OpenTelemetry trace context propagation
//   - Timeout and connection pooling configuration
//   - Error handling for upstream failures (returns 502 Bad Gateway)
//
// Parameters:
//   - targetURL: The base URL of the downstream service (e.g., "http://localhost:8081")
//   - prefix: The path prefix to mount and rewrite (e.g., "/api/v1/users")
//   - targetService: The name of the target service for logging
func proxyHandler(targetURL, prefix, targetService string) http.Handler {
	target, err := url.Parse(targetURL)
	if err != nil {
		slog.Error("invalid target URL", "service", serviceName, "target_url", targetURL, "error", err.Error())
		os.Exit(1)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	// Set timeout on the transport for resilience, wrapped with OpenTelemetry
	baseTransport := &http.Transport{
		ResponseHeaderTimeout: 5 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConnsPerHost:   10,
	}
	// Explicitly pass tracer provider and propagator to ensure trace context propagation
	proxy.Transport = otelhttp.NewTransport(
		baseTransport,
		otelhttp.WithTracerProvider(otel.GetTracerProvider()),
		otelhttp.WithPropagators(otel.GetTextMapPropagator()),
	)

	// Handle proxy errors (timeouts, connection failures)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		correlationID := getCorrelationID(r.Context())
		slog.Error("proxy error",
			"service", serviceName,
			"correlation_id", correlationID,
			"target", targetService,
			"error", err.Error(),
		)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": "upstream service unavailable"})
	}

	// Modify the Director to forward all headers including Authorization, X-Correlation-ID, and trace context
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		// Forward the correlation ID to downstream service
		if correlationID := getCorrelationID(req.Context()); correlationID != "" {
			req.Header.Set("X-Correlation-ID", correlationID)
		}
		// Inject OTel trace context headers (traceparent, tracestate) for distributed tracing
		otel.GetTextMapPropagator().Inject(req.Context(), propagation.HeaderCarrier(req.Header))
	}

	return http.StripPrefix(prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlationID := getCorrelationID(r.Context())
		slog.Info("proxying request",
			"service", serviceName,
			"correlation_id", correlationID,
			"target", targetService,
			"method", r.Method,
			"path", prefix+r.URL.Path,
		)

		r.URL.Path = prefix + r.URL.Path
		r.Host = target.Host
		proxy.ServeHTTP(w, r)
	}))
}

// getEnv retrieves an environment variable value or returns the fallback if not set.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
