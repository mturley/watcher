package watcher

import (
	"context"
	"time"
)

// Loop runs pollFn immediately, then once every interval, until ctx is
// cancelled, at which point it returns ctx.Err(). A pollFn error does NOT
// stop the loop — a transient poll failure must not end watching — so callers
// that want to observe errors should handle them inside pollFn (e.g. log or
// record poller status). Loop is the in-process alternative to OS scheduling:
// use it for a foreground `watch loop` command or as a goroutine inside a
// long-running server.
func Loop(ctx context.Context, interval time.Duration, pollFn func(context.Context) error) error {
	// Immediate first run (unless already cancelled).
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_ = pollFn(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_ = pollFn(ctx)
		}
	}
}
