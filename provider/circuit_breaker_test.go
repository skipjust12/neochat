package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCircuitBreakerClient_PassesThroughOnSuccess(t *testing.T) {
	fake := &FakeClient{Responses: []GenerateResult{{Text: "ok"}, {Text: "ok2"}}}
	cb := NewCircuitBreakerClient(fake, 2, time.Minute)
	ctx := context.Background()

	if _, err := cb.Generate(ctx, "model-a", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := cb.Generate(ctx, "model-a", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.Requests) != 2 {
		t.Errorf("expected 2 calls to reach the underlying client, got %d", len(fake.Requests))
	}
}

func TestCircuitBreakerClient_OpensAfterConsecutiveFailures(t *testing.T) {
	fake := &FakeClient{Err: errors.New("upstream down")}
	cb := NewCircuitBreakerClient(fake, 2, time.Minute)
	ctx := context.Background()

	if _, err := cb.Generate(ctx, "model-a", nil); err == nil {
		t.Fatal("expected an error from the first call")
	}
	if _, err := cb.Generate(ctx, "model-a", nil); err == nil {
		t.Fatal("expected an error from the second call")
	}
	if len(fake.Requests) != 2 {
		t.Fatalf("expected exactly 2 calls to reach the underlying client, got %d", len(fake.Requests))
	}

	_, err := cb.Generate(ctx, "model-a", nil)
	var circuitErr *CircuitOpenError
	if !errors.As(err, &circuitErr) {
		t.Fatalf("expected a *CircuitOpenError, got %v (%T)", err, err)
	}
	if circuitErr.APIModelID != "model-a" {
		t.Errorf("APIModelID = %q, want model-a", circuitErr.APIModelID)
	}
	if len(fake.Requests) != 2 {
		t.Errorf("expected the circuit-open call to skip the underlying client entirely, still got %d calls", len(fake.Requests))
	}
}

func TestCircuitBreakerClient_ResumesAfterCooldown(t *testing.T) {
	fake := &FakeClient{Err: errors.New("upstream down")}
	cb := NewCircuitBreakerClient(fake, 1, time.Minute)
	ctx := context.Background()

	fixedNow := time.Now()
	cb.now = func() time.Time { return fixedNow }

	if _, err := cb.Generate(ctx, "model-a", nil); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := cb.Generate(ctx, "model-a", nil); err == nil {
		t.Fatal("expected a circuit-open error before cooldown elapses")
	}
	if len(fake.Requests) != 1 {
		t.Fatalf("expected only 1 call to reach the underlying client before cooldown, got %d", len(fake.Requests))
	}

	cb.now = func() time.Time { return fixedNow.Add(2 * time.Minute) }
	fake.Err = nil
	fake.Responses = []GenerateResult{{Text: "recovered"}}

	if _, err := cb.Generate(ctx, "model-a", nil); err != nil {
		t.Fatalf("expected the circuit to allow a call through after cooldown, got error: %v", err)
	}
	if len(fake.Requests) != 2 {
		t.Errorf("expected the post-cooldown call to reach the underlying client, got %d total calls", len(fake.Requests))
	}
}

func TestCircuitBreakerClient_SuccessResetsFailureCounter(t *testing.T) {
	fake := &FakeClient{Err: errors.New("upstream down")}
	cb := NewCircuitBreakerClient(fake, 2, time.Minute)
	ctx := context.Background()

	if _, err := cb.Generate(ctx, "model-a", nil); err == nil {
		t.Fatal("expected an error")
	}

	fake.Err = nil
	fake.Responses = []GenerateResult{{Text: "ok"}}
	if _, err := cb.Generate(ctx, "model-a", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fake.Err = errors.New("upstream down again")
	if _, err := cb.Generate(ctx, "model-a", nil); err == nil {
		t.Fatal("expected an error")
	}

	// Only 1 failure since the last success (threshold is 2), so the
	// circuit must still be closed -- this call has to reach the
	// underlying client, not get short-circuited.
	fake.Err = nil
	fake.Responses = append(fake.Responses, GenerateResult{Text: "still going"})
	if _, err := cb.Generate(ctx, "model-a", nil); err != nil {
		t.Fatalf("expected the circuit to still be closed, got error: %v", err)
	}
	if len(fake.Requests) != 4 {
		t.Errorf("expected all 4 calls to reach the underlying client, got %d", len(fake.Requests))
	}
}

// drainStream reads a StreamChunk channel to completion and returns the
// concatenated deltas, the final chunk's Err (if the stream failed), and
// whether a Done chunk was seen.
func drainStream(t *testing.T, ch <-chan StreamChunk) (text string, err error, done bool) {
	t.Helper()
	var sb strings.Builder
	for chunk := range ch {
		sb.WriteString(chunk.Delta)
		if chunk.Err != nil {
			err = chunk.Err
		}
		if chunk.Done {
			done = true
		}
	}
	return sb.String(), err, done
}

func TestCircuitBreakerClient_GenerateStream_PassesThroughOnSuccess(t *testing.T) {
	fake := &FakeClient{Responses: []GenerateResult{{Text: "hello world"}}}
	cb := NewCircuitBreakerClient(fake, 1, time.Minute)

	ch, err := cb.GenerateStream(context.Background(), "model-a", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text, streamErr, done := drainStream(t, ch)
	if streamErr != nil {
		t.Fatalf("unexpected stream error: %v", streamErr)
	}
	if !done {
		t.Error("expected a Done chunk")
	}
	if text != "hello world" {
		t.Errorf("streamed text = %q, want %q", text, "hello world")
	}

	// A second call must still reach the underlying client -- success
	// should not have tripped the circuit.
	fake.Responses = append(fake.Responses, GenerateResult{Text: "again"})
	if _, err := cb.GenerateStream(context.Background(), "model-a", nil); err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
}

func TestCircuitBreakerClient_GenerateStream_OpensAfterFailure(t *testing.T) {
	fake := &FakeClient{Err: errors.New("upstream down")}
	cb := NewCircuitBreakerClient(fake, 1, time.Minute)
	ctx := context.Background()

	ch, err := cb.GenerateStream(ctx, "model-a", nil)
	if err != nil {
		t.Fatalf("unexpected error opening the stream: %v", err)
	}
	_, streamErr, _ := drainStream(t, ch)
	if streamErr == nil {
		t.Fatal("expected the stream to end with an error")
	}

	// The circuit is now open: the next call must be fast-failed with a
	// *CircuitOpenError before ever reaching GenerateStream on fake.
	if _, err := cb.GenerateStream(ctx, "model-a", nil); err == nil {
		t.Fatal("expected an error from the second call")
	} else {
		var circuitErr *CircuitOpenError
		if !errors.As(err, &circuitErr) {
			t.Fatalf("expected a *CircuitOpenError, got %v (%T)", err, err)
		}
	}
	if len(fake.Requests) != 1 {
		t.Errorf("expected the circuit-open call to skip the underlying client entirely, still got %d calls", len(fake.Requests))
	}
}

func TestCircuitBreakerClient_GenerateStream_RequiresUnderlyingStreamingClient(t *testing.T) {
	cb := NewCircuitBreakerClient(nonStreamingClient{}, 1, time.Minute)
	if _, err := cb.GenerateStream(context.Background(), "model-a", nil); err == nil {
		t.Fatal("expected an error when the underlying Client doesn't implement StreamingClient")
	}
}

// nonStreamingClient implements Client but deliberately not StreamingClient.
type nonStreamingClient struct{}

func (nonStreamingClient) Generate(ctx context.Context, apiModelID string, messages []Message) (GenerateResult, error) {
	return GenerateResult{}, nil
}

func TestCircuitBreakerClient_PerModelIsolated(t *testing.T) {
	fake := &FakeClient{Err: errors.New("model-a is down")}
	cb := NewCircuitBreakerClient(fake, 1, time.Minute)
	ctx := context.Background()

	if _, err := cb.Generate(ctx, "model-a", nil); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := cb.Generate(ctx, "model-a", nil); err == nil {
		t.Fatal("expected a circuit-open error for model-a")
	}

	fake.Err = nil
	fake.Responses = []GenerateResult{{Text: "model-b is fine"}}
	if _, err := cb.Generate(ctx, "model-b", nil); err != nil {
		t.Fatalf("expected model-b to succeed independently of model-a's open circuit, got: %v", err)
	}
}
