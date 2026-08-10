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

	// Block, if non-nil, makes Generate wait on this channel (or on ctx
	// being done, whichever comes first) before doing anything else --
	// simulates a slow/hung upstream call for tests that need to verify
	// context cancellation actually stops in-flight work instead of
	// running it to completion. A nil channel (the zero value) means "no
	// blocking", so existing tests that don't set this are unaffected.
	// Nothing ever needs to close/send on it in practice: a canceled ctx
	// is always the way these tests end the block.
	Block chan struct{}

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

func (f *FakeClient) Generate(ctx context.Context, apiModelID string, messages []Message) (GenerateResult, error) {
	f.Requests = append(f.Requests, FakeRequest{APIModelID: apiModelID, Messages: messages})
	if f.Block != nil {
		select {
		case <-ctx.Done():
			return GenerateResult{}, ctx.Err()
		case <-f.Block:
		}
	}
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
