package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func main() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Health endpoints
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "api-gateway"})
	})
	r.Get("/ready", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ready", "service": "api-gateway"})
	})

	// Reverse proxy routes
	userServiceURL := getEnv("USER_SERVICE_URL", "http://localhost:8081")
	productServiceURL := getEnv("PRODUCT_SERVICE_URL", "http://localhost:8082")
	orderServiceURL := getEnv("ORDER_SERVICE_URL", "http://localhost:8083")

	r.Route("/api/v1", func(r chi.Router) {
		r.Mount("/auth", proxyHandler(userServiceURL, "/api/v1/auth"))
		r.Mount("/users", proxyHandler(userServiceURL, "/api/v1/users"))
		r.Mount("/products", proxyHandler(productServiceURL, "/api/v1/products"))
		r.Mount("/orders", proxyHandler(orderServiceURL, "/api/v1/orders"))
	})

	port := getEnv("PORT", "8080")
	log.Printf("API Gateway starting on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}

func proxyHandler(targetURL, prefix string) http.Handler {
	target, err := url.Parse(targetURL)
	if err != nil {
		log.Fatalf("Invalid target URL: %v", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	return http.StripPrefix(prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
