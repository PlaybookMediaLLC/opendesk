package httpx

import (
	"context"
	"sync/atomic"
)

type secretMarkKey struct{}

// WithSecretMark returns a context in which MarkSecretResponse records that the response
// carries a secret shown once, and the flag it sets. Middleware that keeps responses
// (internal/idempotency) installs it and never stores a marked response's body.
func WithSecretMark(ctx context.Context) (context.Context, *atomic.Bool) {
	mark := &atomic.Bool{}
	return context.WithValue(ctx, secretMarkKey{}, mark), mark
}

// MarkSecretResponse records that the response to the request of ctx carries a secret that
// is shown once (an API key, a signing secret): it is answered to this request alone and
// never stored for a replay.
func MarkSecretResponse(ctx context.Context) {
	if mark, ok := ctx.Value(secretMarkKey{}).(*atomic.Bool); ok {
		mark.Store(true)
	}
}
