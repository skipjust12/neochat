package provider

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CircuitBreakerClient wraps a Client and stops calling a specific
// apiModelID for a cooldown period after enough consecutive failures,
// instead of letting every request against a known-bad model separately
// discover the same failure via its own timeout -- the "circuit breaker /
// per-model health tracking" item from README's pre-launch checklist.
//
// State is tracked per apiModelID, not per Client instance. That matters
// here because every catalog provider shares one OpenRouterClient (see
// cmd/server/main.go's Generators map) -- wrapping that single client in
// one CircuitBreakerClient still isolates a failing model's circuit from
// every other model going through the same underlying client.
//
// This is a simplified breaker, not a textbook single-probe half-open
// state machine: once the cooldown passes, calls resume normally for
// everyone, and a success resets the failure counter to zero -- but the
// counter is NOT reset just because time passed, so a single renewed
// failure right after cooldown reopens the circuit immediately rather
// than tolerating a few more tries. Good enough to stop a genuinely dead
// model from being retried by every single request; a stricter one-probe
// gate can be added later if a partially-recovering model turns out to
// need it.
//
// What this deliberately does NOT do: make router.Route pick a different
// model while this one's circuit is open. Generate still returns
// CircuitOpenError, and the caller (server.handle) surfaces it as a
// normal request failure -- actually rerouting around an unhealthy model
// would need router.Route to accept a set of currently-excluded models,
// which is a real API change to router/, not something this type
// attempts. See README "Next steps".
type CircuitBreakerClient struct {
	Client           Client
	FailureThreshold int           // consecutive failures before the circuit opens
	CooldownPeriod   time.Duration // how long the circuit stays open once tripped

	mu     sync.Mutex
	states map[string]*circuitState

	// now is a seam for tests to control time without sleeping; defaults
	// to time.Now via NewCircuitBreakerClient.
	now func() time.Time
}

type circuitState struct {
	consecutiveFailures int
	openUntil           time.Time
}

// NewCircuitBreakerClient wraps client, opening a given model's circuit
// after failureThreshold consecutive failures against it and keeping it
// open for cooldownPeriod before allowing calls through again.
func NewCircuitBreakerClient(client Client, failureThreshold int, cooldownPeriod time.Duration) *CircuitBreakerClient {
	return &CircuitBreakerClient{
		Client:           client,
		FailureThreshold: failureThreshold,
		CooldownPeriod:   cooldownPeriod,
		states:           make(map[string]*circuitState),
		now:              time.Now,
	}
}

// CircuitOpenError is returned instead of calling the underlying Client
// when apiModelID's circuit is currently open.
type CircuitOpenError struct {
	APIModelID string
	RetryAfter time.Duration
}

func (e *CircuitOpenError) Error() string {
	return fmt.Sprintf("provider: circuit open for %q, retry after %s", e.APIModelID, e.RetryAfter)
}

func (c *CircuitBreakerClient) Generate(ctx context.Context, apiModelID string, messages []Message) (GenerateResult, error) {
	now := c.now()

	c.mu.Lock()
	state := c.states[apiModelID]
	if state != nil && now.Before(state.openUntil) {
		retryAfter := state.openUntil.Sub(now)
		c.mu.Unlock()
		return GenerateResult{}, &CircuitOpenError{APIModelID: apiModelID, RetryAfter: retryAfter}
	}
	c.mu.Unlock()

	result, err := c.Client.Generate(ctx, apiModelID, messages)

	c.mu.Lock()
	defer c.mu.Unlock()
	state = c.states[apiModelID]
	if state == nil {
		state = &circuitState{}
		c.states[apiModelID] = state
	}
	if err != nil {
		state.consecutiveFailures++
		if state.consecutiveFailures >= c.FailureThreshold {
			state.openUntil = c.now().Add(c.CooldownPeriod)
		}
	} else {
		state.consecutiveFailures = 0
		state.openUntil = time.Time{}
	}
	return result, err
}
