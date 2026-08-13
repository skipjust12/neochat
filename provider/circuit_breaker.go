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
// Generate still returns CircuitOpenError itself rather than picking a
// different model -- this type has no notion of "the catalog" or
// "routing", only apiModelID strings. Failover happens one layer up:
// server.handle catches CircuitOpenError from Generate and retries
// router.Route with the failed model added to its excludedModelIDs set,
// so the next Route call picks a different, healthy candidate.
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
	if err := c.checkOpen(apiModelID); err != nil {
		return GenerateResult{}, err
	}

	result, err := c.Client.Generate(ctx, apiModelID, messages)
	c.recordResult(apiModelID, err)
	return result, err
}

// GenerateStream is Generate's streaming counterpart: the same open-circuit
// fast-fail check up front, delegating to the underlying Client's
// GenerateStream (it must implement StreamingClient, or this errors) if
// the circuit is closed, and the same failure/success bookkeeping once the
// stream ends -- observed here by watching for a StreamChunk with Err set
// vs. one with Done set, since nothing else marks the terminal chunk of a
// stream.
func (c *CircuitBreakerClient) GenerateStream(ctx context.Context, apiModelID string, messages []Message) (<-chan StreamChunk, error) {
	if err := c.checkOpen(apiModelID); err != nil {
		return nil, err
	}

	streamingClient, ok := c.Client.(StreamingClient)
	if !ok {
		return nil, fmt.Errorf("provider: circuit breaker's underlying %T does not implement StreamingClient", c.Client)
	}
	upstream, err := streamingClient.GenerateStream(ctx, apiModelID, messages)
	if err != nil {
		c.recordResult(apiModelID, err)
		return nil, err
	}

	out := make(chan StreamChunk)
	go func() {
		defer close(out)
		// Same reasoning as OpenRouterClient.GenerateStream's recover: a
		// panic in a goroutine nobody is recovering for takes the process
		// down, so it becomes a stream error for this one request instead.
		// Counted as a failure against the model's circuit, like any other
		// error from it would be.
		defer func() {
			if rec := recover(); rec != nil {
				err := fmt.Errorf("provider: panic in circuit breaker stream relay for %q: %v", apiModelID, rec)
				c.recordResult(apiModelID, err)
				select {
				case out <- StreamChunk{Err: err}:
				case <-ctx.Done():
				}
			}
		}()
		for chunk := range upstream {
			out <- chunk
			if chunk.Err != nil {
				c.recordResult(apiModelID, chunk.Err)
				return
			}
			if chunk.Done {
				c.recordResult(apiModelID, nil)
				return
			}
		}
	}()
	return out, nil
}

// checkOpen returns a *CircuitOpenError without touching the underlying
// Client if apiModelID's circuit is currently open, nil otherwise.
func (c *CircuitBreakerClient) checkOpen(apiModelID string) error {
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.states[apiModelID]
	if state != nil && now.Before(state.openUntil) {
		return &CircuitOpenError{APIModelID: apiModelID, RetryAfter: state.openUntil.Sub(now)}
	}
	return nil
}

// recordResult updates apiModelID's consecutive-failure count and,
// crossing FailureThreshold, opens its circuit for CooldownPeriod -- or
// resets both on a nil err (success).
func (c *CircuitBreakerClient) recordResult(apiModelID string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.states[apiModelID]
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
}
