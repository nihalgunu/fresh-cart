package resilience

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"net/http"
	"time"
)

// RetryConfig holds the configuration for retry logic
type RetryConfig struct {
	MaxRetries   int           // Maximum number of retries
	InitialDelay time.Duration // Initial delay between retries
	MaxDelay     time.Duration // Maximum delay between retries
	MaxJitter    time.Duration // Maximum random jitter added to each delay
}

// DefaultRetryConfig returns the default retry configuration
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:   3,
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     2 * time.Second,
		MaxJitter:    100 * time.Millisecond,
	}
}

// RetryableError wraps an error with information about whether it's retryable
type RetryableError struct {
	Err       error
	Retryable bool
	StatusCode int
}

func (e *RetryableError) Error() string {
	return e.Err.Error()
}

func (e *RetryableError) Unwrap() error {
	return e.Err
}

// IsRetryable checks if an error is retryable
// Returns true for 5xx errors and connection errors, false for 4xx errors
func IsRetryable(err error, statusCode int) bool {
	if err != nil {
		// Connection errors are retryable
		return true
	}
	// Only retry 5xx errors, not 4xx
	return statusCode >= 500
}

// Retryer handles retry logic with exponential backoff and jitter
type Retryer struct {
	config RetryConfig
}

// NewRetryer creates a new Retryer with the given configuration
func NewRetryer(config RetryConfig) *Retryer {
	return &Retryer{config: config}
}

// HTTPResponse represents the result of an HTTP call
type HTTPResponse struct {
	Response   *http.Response
	StatusCode int
	Err        error
}

// ExecuteHTTP executes an HTTP call with retry logic
// The fn function should return the response, status code, and any error
func (r *Retryer) ExecuteHTTP(ctx context.Context, target, path string, fn func() (*http.Response, error)) (*http.Response, error) {
	var lastErr error
	var lastStatusCode int

	for attempt := 0; attempt <= r.config.MaxRetries; attempt++ {
		// Check context before attempting
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		resp, err := fn()

		if err == nil && resp != nil {
			statusCode := resp.StatusCode
			// Success or non-retryable client error
			if statusCode < 500 {
				return resp, nil
			}
			// Server error - close body and retry
			lastStatusCode = statusCode
			lastErr = errors.New("server error: " + resp.Status)
			resp.Body.Close()
		} else {
			lastErr = err
			if resp != nil {
				lastStatusCode = resp.StatusCode
			}
		}

		// Don't retry if we've exhausted attempts
		if attempt >= r.config.MaxRetries {
			break
		}

		// Don't retry 4xx errors
		if lastStatusCode >= 400 && lastStatusCode < 500 {
			if resp != nil {
				return resp, nil
			}
			return nil, lastErr
		}

		// Calculate delay with exponential backoff
		delay := r.calculateDelay(attempt)

		slog.Info("retrying request",
			"service", "order-service",
			"attempt", attempt+2, // Next attempt number (1-indexed, +1 for next)
			"delay_ms", delay.Milliseconds(),
			"target", target,
			"path", path,
		)

		// Wait before retrying
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}

	return nil, lastErr
}

// calculateDelay calculates the delay for a retry attempt using exponential backoff with jitter
func (r *Retryer) calculateDelay(attempt int) time.Duration {
	// Exponential backoff: initialDelay * 2^attempt
	delay := r.config.InitialDelay * (1 << attempt)

	// Cap at max delay
	if delay > r.config.MaxDelay {
		delay = r.config.MaxDelay
	}

	// Add random jitter (0 to MaxJitter)
	jitter := time.Duration(rand.Int63n(int64(r.config.MaxJitter)))
	delay += jitter

	return delay
}
