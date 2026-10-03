package provider

import (
	"context"
	"strings"
)

type outputLimitKey struct{}

// WithOutputLimit applies a per-call ceiling without mutating a shared client.
func WithOutputLimit(ctx context.Context, tokens int) context.Context {
	return context.WithValue(ctx, outputLimitKey{}, tokens)
}

func outputLimit(ctx context.Context, configured int) int {
	if limit, ok := ctx.Value(outputLimitKey{}).(int); ok && limit > 0 && (configured <= 0 || limit < configured) {
		return limit
	}
	return configured
}

type apiKeyKey struct{}

// WithAPIKey attaches the requesting user's own vendor key to ctx, so a
// shared client calls the vendor on that user's account. The key is never
// stored server-side -- it lives only as long as the request.
func WithAPIKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, apiKeyKey{}, strings.TrimSpace(key))
}

func apiKeyFromContext(ctx context.Context) string {
	key, _ := ctx.Value(apiKeyKey{}).(string)
	return key
}

// Reasoning is the per-call reasoning setting. Effort is one of
// "low", "medium", "high", "xhigh", "max" (already normalized for the
// model -- see server's reasoningFor); Adaptive selects the
// type=adaptive + effort_level form newer Claude models require.
type Reasoning struct {
	Effort   string
	Adaptive bool
}

type reasoningKey struct{}

// WithReasoning attaches a reasoning setting to ctx for the next
// generation call. Calls without one send no reasoning block at all, which
// leaves the model on its own default.
func WithReasoning(ctx context.Context, r Reasoning) context.Context {
	return context.WithValue(ctx, reasoningKey{}, r)
}

func reasoningFromContext(ctx context.Context) (Reasoning, bool) {
	r, ok := ctx.Value(reasoningKey{}).(Reasoning)
	return r, ok && r.Effort != ""
}
