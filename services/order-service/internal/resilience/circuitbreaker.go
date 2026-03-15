package resilience

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Circuit breaker states
const (
	StateClosed   = 0 // Normal operation
	StateHalfOpen = 1 // Testing recovery
	StateOpen     = 2 // Failing, reject fast
)

var stateNames = map[int]string{
	StateClosed:   "closed",
	StateHalfOpen: "half-open",
	StateOpen:     "open",
}

// ErrCircuitOpen is returned when the circuit breaker is open
var ErrCircuitOpen = errors.New("circuit breaker is open")

// Shared Prometheus gauge for all circuit breakers
var circuitBreakerStateGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "circuit_breaker_state",
		Help: "Circuit breaker state (0=closed, 1=half-open, 2=open)",
	},
	[]string{"target"},
)

func init() {
	prometheus.MustRegister(circuitBreakerStateGauge)
}

// CircuitBreakerConfig holds the configuration for a circuit breaker
type CircuitBreakerConfig struct {
	FailureThreshold int           // Consecutive failures to open
	SuccessThreshold int           // Consecutive successes in half-open to close
	OpenTimeout      time.Duration // Time before transitioning from open to half-open
}

// DefaultConfig returns the default circuit breaker configuration
func DefaultConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		FailureThreshold: 5,
		SuccessThreshold: 3,
		OpenTimeout:      10 * time.Second,
	}
}

// CircuitBreaker implements the circuit breaker pattern
type CircuitBreaker struct {
	name   string
	config CircuitBreakerConfig

	mu                   sync.RWMutex
	state                int
	consecutiveFailures  int
	consecutiveSuccesses int
	lastFailureTime      time.Time
}

// NewCircuitBreaker creates a new circuit breaker with the given configuration
func NewCircuitBreaker(name string, config CircuitBreakerConfig) *CircuitBreaker {
	cb := &CircuitBreaker{
		name:   name,
		config: config,
		state:  StateClosed,
	}

	// Initialize the metric to closed state
	circuitBreakerStateGauge.WithLabelValues(name).Set(float64(StateClosed))

	return cb
}

// Execute runs the given function through the circuit breaker
func (cb *CircuitBreaker) Execute(fn func() error) error {
	if !cb.allowRequest() {
		return ErrCircuitOpen
	}

	err := fn()

	if err != nil {
		cb.recordFailure()
	} else {
		cb.recordSuccess()
	}

	return err
}

// allowRequest checks if a request should be allowed through
func (cb *CircuitBreaker) allowRequest() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		return true
	case StateOpen:
		// Check if we should transition to half-open
		if time.Since(cb.lastFailureTime) >= cb.config.OpenTimeout {
			cb.transitionTo(StateHalfOpen)
			return true
		}
		return false
	case StateHalfOpen:
		return true
	default:
		return true
	}
}

// recordFailure records a failed request
func (cb *CircuitBreaker) recordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveFailures++
	cb.consecutiveSuccesses = 0
	cb.lastFailureTime = time.Now()

	switch cb.state {
	case StateClosed:
		if cb.consecutiveFailures >= cb.config.FailureThreshold {
			cb.transitionTo(StateOpen)
		}
	case StateHalfOpen:
		// Any failure in half-open returns to open
		cb.transitionTo(StateOpen)
	}
}

// recordSuccess records a successful request
func (cb *CircuitBreaker) recordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveSuccesses++
	cb.consecutiveFailures = 0

	if cb.state == StateHalfOpen {
		if cb.consecutiveSuccesses >= cb.config.SuccessThreshold {
			cb.transitionTo(StateClosed)
		}
	}
}

// transitionTo changes the circuit breaker state (must be called with lock held)
func (cb *CircuitBreaker) transitionTo(newState int) {
	if cb.state == newState {
		return
	}

	oldState := cb.state
	cb.state = newState

	// Reset counters on state change
	cb.consecutiveFailures = 0
	cb.consecutiveSuccesses = 0

	// Update Prometheus metric
	circuitBreakerStateGauge.WithLabelValues(cb.name).Set(float64(newState))

	slog.Info("circuit breaker state change",
		"service", "order-service",
		"msg", "circuit breaker state change",
		"from", stateNames[oldState],
		"to", stateNames[newState],
		"target", cb.name,
	)
}

// State returns the current state of the circuit breaker
func (cb *CircuitBreaker) State() int {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// StateName returns the name of the current state
func (cb *CircuitBreaker) StateName() string {
	return stateNames[cb.State()]
}
