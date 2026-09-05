package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// countingClient records every Generate/GenerateStream call and replays a
// scripted error sequence, so a test can assert on how many attempts were
// actually made.
type countingClient struct {
	errs  []error // one per call; a nil entry succeeds
	calls int
}

func (c *countingClient) Generate(context.Context, string, []Message) (GenerateResult, error) {
	i := c.calls
	c.calls++
	if i < len(c.errs) && c.errs[i] != nil {
		return GenerateResult{}, c.errs[i]
	}
	return GenerateResult{Text: "ok", InputTokens: 1, OutputTokens: 1}, nil
}

func (c *countingClient) GenerateStream(context.Context, string, []Message) (<-chan StreamChunk, error) {
	i := c.calls
	c.calls++
	if i < len(c.errs) && c.errs[i] != nil {
		return nil, c.errs[i]
	}
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Done: true, Final: GenerateResult{Text: "ok"}}
	close(ch)
	return ch, nil
}

// newTestRetryClient wires a RetryClient whose backoff never actually
// sleeps, recording the delays it would have waited instead, and whose
// jitter is fixed so those delays are deterministic.
func newTestRetryClient(inner Client, maxAttempts int) (*RetryClient, *[]time.Duration) {
	var slept []time.Duration
	c := &RetryClient{
		Client:      inner,
		MaxAttempts: maxAttempts,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    time.Second,
		jitter:      func() float64 { return 0.5 }, // no jitter: exactly 1.0x
		sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
	}
	return c, &slept
}

func TestRetryClient_RetriesRateLimitThenSucceeds(t *testing.T) {
	inner := &countingClient{errs: []error{
		&StatusError{StatusCode: http.StatusTooManyRequests},
		&StatusError{StatusCode: http.StatusTooManyRequests},
		nil,
	}}
	c, slept := newTestRetryClient(inner, 3)

	result, err := c.Generate(context.Background(), "m", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "ok" {
		t.Errorf("Text = %q, want ok", result.Text)
	}
	if inner.calls != 3 {
		t.Errorf("inner called %d times, want 3", inner.calls)
	}
	// 100ms then 200ms -- doubling, with jitter pinned to 1.0x.
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}
	if len(*slept) != len(want) {
		t.Fatalf("slept %v, want %v", *slept, want)
	}
	for i := range want {
		if (*slept)[i] != want[i] {
			t.Errorf("backoff %d = %s, want %s", i, (*slept)[i], want[i])
		}
	}
}

func TestRetryClient_GivesUpAfterMaxAttempts(t *testing.T) {
	inner := &countingClient{errs: []error{
		&StatusError{StatusCode: http.StatusTooManyRequests},
		&StatusError{StatusCode: http.StatusTooManyRequests},
		&StatusError{StatusCode: http.StatusTooManyRequests},
	}}
	c, _ := newTestRetryClient(inner, 3)

	_, err := c.Generate(context.Background(), "m", nil)
	if err == nil {
		t.Fatal("expected an error after exhausting attempts")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("err = %v, want the underlying 429 StatusError", err)
	}
	if inner.calls != 3 {
		t.Errorf("inner called %d times, want exactly MaxAttempts (3)", inner.calls)
	}
}

// TestRetryClient_DoesNotRetryNonRetryable is the important half of the
// policy: a request the vendor rejected on its merits, or one it may have
// actually processed, must be sent exactly once. Retrying a 400 wastes
// time; retrying a 504 risks paying twice for one generation (see
// StatusError.Retryable).
func TestRetryClient_DoesNotRetryNonRetryable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"bad request", http.StatusBadRequest},
		{"unauthorized", http.StatusUnauthorized},
		{"payment required", http.StatusPaymentRequired},
		{"gateway timeout may already have been billed", http.StatusGatewayTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := &countingClient{errs: []error{&StatusError{StatusCode: tc.status}}}
			c, _ := newTestRetryClient(inner, 3)

			if _, err := c.Generate(context.Background(), "m", nil); err == nil {
				t.Fatal("expected an error")
			}
			if inner.calls != 1 {
				t.Errorf("status %d: inner called %d times, want exactly 1", tc.status, inner.calls)
			}
		})
	}
}

// TestRetryClient_DoesNotRetryPlainErrors covers everything that isn't an
// HTTP status at all -- a JSON parse failure, a context cancellation, a
// panic converted to an error. None of those get better by being resent.
func TestRetryClient_DoesNotRetryPlainErrors(t *testing.T) {
	inner := &countingClient{errs: []error{errors.New("parse response: unexpected EOF")}}
	c, _ := newTestRetryClient(inner, 3)

	if _, err := c.Generate(context.Background(), "m", nil); err == nil {
		t.Fatal("expected an error")
	}
	if inner.calls != 1 {
		t.Errorf("inner called %d times, want exactly 1", inner.calls)
	}
}

func TestRetryClient_HonorsRetryAfterOverComputedBackoff(t *testing.T) {
	inner := &countingClient{errs: []error{
		&StatusError{StatusCode: http.StatusTooManyRequests, RetryAfter: 5 * time.Second},
		nil,
	}}
	c, slept := newTestRetryClient(inner, 3)

	if _, err := c.Generate(context.Background(), "m", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*slept) != 1 || (*slept)[0] != 5*time.Second {
		t.Errorf("slept %v, want a single 5s wait from the vendor's Retry-After (not the 100ms base delay)", *slept)
	}
}

func TestRetryClient_StopsWhenContextDies(t *testing.T) {
	inner := &countingClient{errs: []error{
		&StatusError{StatusCode: http.StatusTooManyRequests},
		&StatusError{StatusCode: http.StatusTooManyRequests},
	}}
	c, _ := newTestRetryClient(inner, 5)
	c.sleep = func(context.Context, time.Duration) error { return context.Canceled }

	_, err := c.Generate(context.Background(), "m", nil)
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Errorf("err = %v, want the vendor's StatusError rather than the interrupted wait", err)
	}
	if inner.calls != 1 {
		t.Errorf("inner called %d times, want 1 (no further attempts once the context is done)", inner.calls)
	}
}

// TestRetryClient_StreamRetriesOnlyBeforeStreamStarts pins down the
// streaming rule: an error returned by GenerateStream itself means nothing
// was emitted, so it can be retried; anything after that is the caller's
// to deal with, since resending would duplicate already-billed output.
func TestRetryClient_StreamRetriesOnlyBeforeStreamStarts(t *testing.T) {
	inner := &countingClient{errs: []error{
		&StatusError{StatusCode: http.StatusTooManyRequests},
		nil,
	}}
	c, _ := newTestRetryClient(inner, 3)

	ch, err := c.GenerateStream(context.Background(), "m", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inner.calls != 2 {
		t.Errorf("inner called %d times, want 2 (one rate-limited, one successful)", inner.calls)
	}
	var got GenerateResult
	for chunk := range ch {
		if chunk.Done {
			got = chunk.Final
		}
	}
	if got.Text != "ok" {
		t.Errorf("final text = %q, want ok", got.Text)
	}
}

func TestParseRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"absent", "", 0},
		{"seconds", "30", 30 * time.Second},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"garbage", "soon", 0},
		{"past http date", "Mon, 02 Jan 2006 15:04:05 GMT", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.value != "" {
				h.Set("Retry-After", tc.value)
			}
			if got := parseRetryAfter(h); got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %s, want %s", tc.value, got, tc.want)
			}
		})
	}
}

// TestOpenRouterClient_Generate_ReturnsStatusError checks the real client
// produces the typed error RetryClient depends on, including the
// Retry-After the vendor sent -- without this wiring, every retry
// decision above would silently fall through to "not retryable".
func TestOpenRouterClient_Generate_ReturnsStatusError(t *testing.T) {
	srv := newRateLimitedServer(t)
	defer srv.Close()

	c := NewOpenRouterClient("test-key")
	c.baseURL = srv.URL

	_, err := c.Generate(context.Background(), "m", []Message{{Role: "user", Content: "hi"}})
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v (%T), want a *StatusError", err, err)
	}
	if statusErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", statusErr.StatusCode)
	}
	if statusErr.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %s, want 7s", statusErr.RetryAfter)
	}
	if !statusErr.Retryable() {
		t.Error("a 429 must be retryable")
	}
}

func TestOpenRouterClient_GenerateStream_ReturnsStatusError(t *testing.T) {
	srv := newRateLimitedServer(t)
	defer srv.Close()

	c := NewOpenRouterClient("test-key")
	c.baseURL = srv.URL

	_, err := c.GenerateStream(context.Background(), "m", []Message{{Role: "user", Content: "hi"}})
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v (%T), want a *StatusError", err, err)
	}
	if statusErr.StatusCode != http.StatusTooManyRequests || statusErr.RetryAfter != 7*time.Second {
		t.Errorf("got status=%d retryAfter=%s, want 429/7s", statusErr.StatusCode, statusErr.RetryAfter)
	}
}

// newRateLimitedServer is a stand-in vendor that always rate-limits, with
// a Retry-After header, for the two StatusError wiring tests above.
func newRateLimitedServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","code":429}}`))
	}))
}
