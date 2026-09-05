package provider

import "context"

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
