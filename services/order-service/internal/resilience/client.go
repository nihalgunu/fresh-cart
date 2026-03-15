package resilience

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// ResilientClient wraps HTTP calls with circuit breaker, retry, and timeout
type ResilientClient struct {
	httpClient     *http.Client
	circuitBreaker *CircuitBreaker
	retryer        *Retryer
	target         string
}

// NewResilientClient creates a new resilient HTTP client
func NewResilientClient(target string, timeout time.Duration, cbConfig CircuitBreakerConfig, retryConfig RetryConfig) *ResilientClient {
	return &ResilientClient{
		httpClient: &http.Client{
			Timeout: timeout,
		},
		circuitBreaker: NewCircuitBreaker(target, cbConfig),
		retryer:        NewRetryer(retryConfig),
		target:         target,
	}
}

// Do executes an HTTP request with resilience patterns
// The request flow is: request → retry(circuit_breaker(http_call))
func (c *ResilientClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	path := req.URL.Path

	// Wrap the HTTP call with circuit breaker, then retry
	return c.retryer.ExecuteHTTP(ctx, c.target, path, func() (*http.Response, error) {
		var resp *http.Response
		var httpErr error

		// Execute through circuit breaker
		cbErr := c.circuitBreaker.Execute(func() error {
			var err error
			resp, err = c.httpClient.Do(req.Clone(ctx))
			if err != nil {
				httpErr = err
				return err
			}
			// Treat 5xx as circuit breaker failure
			if resp.StatusCode >= 500 {
				return fmt.Errorf("server error: %d", resp.StatusCode)
			}
			return nil
		})

		// Handle circuit breaker open
		if cbErr == ErrCircuitOpen {
			return nil, cbErr
		}

		// Return connection errors
		if httpErr != nil {
			// Check if it's a timeout
			if ctx.Err() == context.DeadlineExceeded {
				slog.Warn("request timeout",
					"service", "order-service",
					"msg", "request timeout",
					"target", c.target,
					"timeout_seconds", 5,
				)
			}
			return nil, httpErr
		}

		return resp, nil
	})
}

// GetCircuitBreakerState returns the current state of the circuit breaker
func (c *ResilientClient) GetCircuitBreakerState() int {
	return c.circuitBreaker.State()
}
