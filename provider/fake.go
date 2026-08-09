package provider

import "context"

// FakeClient is a scripted, no-network Client for tests -- classifier/ and
// server/ tests use this instead of hitting a real vendor API.
type FakeClient struct {
	// Responses is consumed in order, one per Generate call. Calling
	// Generate more times than there are entries panics -- a test that
	// does this has a bug worth surfacing loudly, not a case to handle
	// gracefully.
	Responses []GenerateResult
	Err       error // if set, every call returns this error instead

	calls int
	// Requests records every call's arguments, for tests that want to
	// assert on what was sent (e.g. that the classifier's system prompt
	// was actually included).
	Requests []FakeRequest
}

type FakeRequest struct {
	APIModelID string
	Messages   []Message
}

func (f *FakeClient) Generate(_ context.Context, apiModelID string, messages []Message) (GenerateResult, error) {
	f.Requests = append(f.Requests, FakeRequest{APIModelID: apiModelID, Messages: messages})
	if f.Err != nil {
		return GenerateResult{}, f.Err
	}
	if f.calls >= len(f.Responses) {
		panic("provider: FakeClient.Generate called more times than Responses were scripted")
	}
	resp := f.Responses[f.calls]
	f.calls++
	return resp, nil
}
