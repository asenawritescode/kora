package kernel

import (
	"context"

	"github.com/asenawritescode/kora/internal/script"
)

// WithPreview returns ctx marked as a preview. A command run under it validates, guards and
// names its record as Execute would, then rolls back: nothing is kept. Callers that persist
// their own intent or receipt rows must not write them under a preview context. Script hooks
// run under it, and any outbound HTTP they try is refused.
func WithPreview(ctx context.Context) context.Context {
	return script.WithPreview(ctx)
}

// IsPreview reports whether ctx is a preview context.
func IsPreview(ctx context.Context) bool {
	return script.IsPreview(ctx)
}
