package provider

import (
	"context"
	"errors"
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
