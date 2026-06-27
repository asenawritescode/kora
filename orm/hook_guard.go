package orm

import (
	"context"
	"fmt"
)

type ctxKey string

const hookDepthKey ctxKey = "kora_hook_depth"
const maxHookDepth = 10

// incHookDepth increments the hook depth counter in the context.
// Returns an error if the depth exceeds the limit.
func incHookDepth(ctx context.Context) (context.Context, error) {
	depth := hookDepthFromCtx(ctx)
	if depth >= maxHookDepth {
		return ctx, fmt.Errorf("max hook depth (%d) exceeded — possible infinite recursion", maxHookDepth)
	}
	return context.WithValue(ctx, hookDepthKey, depth+1), nil
}

func hookDepthFromCtx(ctx context.Context) int {
	if v, ok := ctx.Value(hookDepthKey).(int); ok {
		return v
	}
	return 0
}
