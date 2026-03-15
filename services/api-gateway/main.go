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

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

const serviceName = "api-gateway"

type contextKey string

const (
	correlationIDKey contextKey = "correlation_id"
	userIDKey        contextKey = "user_id"
	userEmailKey     contextKey = "user_email"
)

var (
	redisClient *redis.Client

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
	if err := http.ListenAndServe(":"+port, r); err != nil {
		slog.Error("server failed", "service", serviceName, "error", err.Error())
		os.Exit(1)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": serviceName})
}

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

func proxyHandler(targetURL, prefix, targetService string) http.Handler {
	target, err := url.Parse(targetURL)
	if err != nil {
		slog.Error("invalid target URL", "service", serviceName, "target_url", targetURL, "error", err.Error())
		os.Exit(1)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	// Modify the Director to forward all headers including Authorization and X-Correlation-ID
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		// Forward the correlation ID to downstream service
		if correlationID := getCorrelationID(req.Context()); correlationID != "" {
			req.Header.Set("X-Correlation-ID", correlationID)
		}
		// Authorization header is already present and will be forwarded automatically
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

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
