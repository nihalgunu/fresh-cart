package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

const serviceName = "api-gateway"

type contextKey string

const correlationIDKey contextKey = "correlation_id"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	r := chi.NewRouter()
	r.Use(correlationIDMiddleware)
	r.Use(requestLoggingMiddleware)
	r.Use(middleware.Recoverer)

	// Health endpoints
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": serviceName})
	})
	r.Get("/ready", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ready", "service": serviceName})
	})

	// Reverse proxy routes
	userServiceURL := getEnv("USER_SERVICE_URL", "http://localhost:8081")
	productServiceURL := getEnv("PRODUCT_SERVICE_URL", "http://localhost:8082")
	orderServiceURL := getEnv("ORDER_SERVICE_URL", "http://localhost:8083")

	r.Route("/api/v1", func(r chi.Router) {
		r.Mount("/auth", proxyHandler(userServiceURL, "/api/v1/auth", "user-service"))
		r.Mount("/users", proxyHandler(userServiceURL, "/api/v1/users", "user-service"))
		r.Mount("/products", proxyHandler(productServiceURL, "/api/v1/products", "product-service"))
		r.Mount("/orders", proxyHandler(orderServiceURL, "/api/v1/orders", "order-service"))
	})

	port := getEnv("PORT", "8000")
	slog.Info("api-gateway starting", "service", serviceName, "port", port)
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

func proxyHandler(targetURL, prefix, targetService string) http.Handler {
	target, err := url.Parse(targetURL)
	if err != nil {
		slog.Error("invalid target URL", "service", serviceName, "target_url", targetURL, "error", err.Error())
		os.Exit(1)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	// Modify the Director to forward correlation ID
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		// Forward the correlation ID to downstream service
		if correlationID := getCorrelationID(req.Context()); correlationID != "" {
			req.Header.Set("X-Correlation-ID", correlationID)
		}
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
