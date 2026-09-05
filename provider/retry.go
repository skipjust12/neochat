package provider

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

// StatusError is a vendor call that came back with a non-2xx HTTP status.
// It exists so a caller can tell a rate limit apart from a bad request
// without string-matching an error message -- RetryClient is the caller
// that needs that, but the type is part of the Client contract, not
// RetryClient's private business: any Client implementation talking HTTP
// should return one of these.
type StatusError struct {
	StatusCode int

	// RetryAfter is the vendor's own Retry-After header, parsed; zero when
	// absent or unparseable. Honored over any computed backoff, since the
	// vendor knows better than we do when it will accept traffic again.
	RetryAfter time.Duration

	// Body is the raw response body, kept for the error message. Never
	// shown to end users -- server.clientErrorMessage replaces it with a
	// generic message (see audit.md's error-leakage finding).
	Body string
}

func (e *StatusError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("provider: vendor returned status %d (retry after %s): %s", e.StatusCode, e.RetryAfter, e.Body)
	}
	return fmt.Sprintf("provider: vendor returned status %d: %s", e.StatusCode, e.Body)
}

// Retryable reports whether resending this exact request is both likely
// to help and safe to do.
//
// "Safe" is the load-bearing half, because a chat completion is not
// idempotent: retrying a request the vendor actually processed means
// paying for the generation twice. So this is deliberately narrower than
// the usual "retry every 5xx":
//
//   - 429 always retries. A rate limit is a refusal, so nothing was
//     generated and nothing was billed.
//   - 500/502/503 do NOT retry: the upstream may have generated and billed
//     before an intermediary returned the error.
//   - 504 does NOT retry. A gateway timeout is the one status that
//     plausibly means the upstream model did the work (and billed for
//     it) and only the response was lost on the way back; retrying that
//     buys a second bill for an answer we already paid for.
func (e *StatusError) Retryable() bool {
	switch e.StatusCode {
	case http.StatusTooManyRequests: // A refusal before generation, safe to retry.
		return true
	default:
		return false
	}
}

// parseRetryAfter reads a Retry-After header in either of the two forms
// RFC 9110 allows: delay-seconds, or an HTTP-date. Returns 0 for an
// absent, malformed, or already-past value, which callers treat as "no
// vendor guidance, use the computed backoff".
func parseRetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// RetryClient wraps a Client and resends requests the vendor refused in a
// way that is likely to succeed shortly (see StatusError.Retryable),
// backing off exponentially with jitter between attempts.
//
// This sits *inside* CircuitBreakerClient in cmd/server, not outside it:
// the breaker should count a model as having failed once a request has
// genuinely given up, not once per intermediate attempt. Wrapped the
// other way round, a single burst of rate limiting would trip the breaker
// and take the model out for everyone -- turning a recoverable blip into
// an outage, which is the opposite of what both wrappers are for.
//
// README records real 429s from OpenRouter during live testing, and
// before this a single one of those failed the user's request outright.
type RetryClient struct {
	Client Client

	// MaxAttempts is the total number of tries, first included. 1 disables
	// retrying. Zero uses defaultMaxAttempts.
	MaxAttempts int

	// BaseDelay is the first backoff interval; each subsequent attempt
	// doubles it, capped at MaxDelay. Zero values use the defaults.
	BaseDelay time.Duration
	MaxDelay  time.Duration

	// sleep is a seam for tests, which need to exercise the backoff
	// sequence without actually waiting it out. Nil uses realSleep.
	sleep func(ctx context.Context, d time.Duration) error

	// jitter returns a fraction in [0,1) used to spread retries out so a
	// burst of requests rate-limited together doesn't come back in
	// lockstep and rate-limit itself again. Nil uses rand.Float64.
	jitter func() float64
}

const (
	defaultMaxAttempts = 3
	defaultBaseDelay   = 500 * time.Millisecond
	defaultMaxDelay    = 8 * time.Second
)

// NewRetryClient wraps client with the default retry policy.
func NewRetryClient(client Client) *RetryClient {
	return &RetryClient{Client: client}
}

func (c *RetryClient) maxAttempts() int {
	if c.MaxAttempts > 0 {
		return c.MaxAttempts
	}
	return defaultMaxAttempts
}

func (c *RetryClient) baseDelay() time.Duration {
	if c.BaseDelay > 0 {
		return c.BaseDelay
	}
	return defaultBaseDelay
}

func (c *RetryClient) maxDelay() time.Duration {
	if c.MaxDelay > 0 {
		return c.MaxDelay
	}
	return defaultMaxDelay
}

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// backoffFor returns how long to wait before attempt+1 (attempt is
// 1-based). A vendor-supplied Retry-After wins outright; otherwise it is
// BaseDelay doubled per attempt, capped at MaxDelay, with up to ±25%
// jitter applied.
func (c *RetryClient) backoffFor(attempt int, err *StatusError) time.Duration {
	if err != nil && err.RetryAfter > 0 {
		return err.RetryAfter
	}

	delay := c.baseDelay() << (attempt - 1)
	if delay > c.maxDelay() || delay <= 0 { // <= 0 guards the shift overflowing
		delay = c.maxDelay()
	}

	jitterFn := c.jitter
	if jitterFn == nil {
		jitterFn = rand.Float64
	}
	// (jitter*0.5 - 0.25) spreads the result across [0.75d, 1.25d).
	return time.Duration(float64(delay) * (0.75 + jitterFn()*0.5))
}

// retryable reports whether err is a StatusError worth another attempt,
// returning it too so the caller can read its Retry-After.
func retryable(err error) (*StatusError, bool) {
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		return nil, false
	}
	return statusErr, statusErr.Retryable()
}

func (c *RetryClient) Generate(ctx context.Context, apiModelID string, messages []Message) (GenerateResult, error) {
	sleep := c.sleep
	if sleep == nil {
		sleep = realSleep
	}

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts(); attempt++ {
		result, err := c.Client.Generate(ctx, apiModelID, messages)
		if err == nil {
			return result, nil
		}
		lastErr = err

		statusErr, ok := retryable(err)
		if !ok || attempt == c.maxAttempts() {
			return GenerateResult{}, err
		}
		if sleepErr := sleep(ctx, c.backoffFor(attempt, statusErr)); sleepErr != nil {
			// Context died while backing off -- report the vendor error
			// that got us here, not the wait being interrupted.
			return GenerateResult{}, lastErr
		}
	}
	return GenerateResult{}, lastErr
}

// GenerateStream retries only failures that happen before the stream
// starts producing -- that is, an error returned by GenerateStream
// itself. Once the channel exists the vendor has accepted the request and
// may already have emitted (and billed for) tokens, so a mid-stream error
// is passed straight through: resending would duplicate output the caller
// has already seen, on top of paying for it twice.
func (c *RetryClient) GenerateStream(ctx context.Context, apiModelID string, messages []Message) (<-chan StreamChunk, error) {
	streamingClient, ok := c.Client.(StreamingClient)
	if !ok {
		return nil, fmt.Errorf("provider: retry client's underlying %T does not implement StreamingClient", c.Client)
	}

	sleep := c.sleep
	if sleep == nil {
		sleep = realSleep
	}

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts(); attempt++ {
		ch, err := streamingClient.GenerateStream(ctx, apiModelID, messages)
		if err == nil {
			return ch, nil
		}
		lastErr = err

		statusErr, ok := retryable(err)
		if !ok || attempt == c.maxAttempts() {
			return nil, err
		}
		if sleepErr := sleep(ctx, c.backoffFor(attempt, statusErr)); sleepErr != nil {
			return nil, lastErr
		}
	}
	return nil, lastErr
}
