package watcher

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoopCallsImmediatelyAndTicks(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Loop(ctx, 20*time.Millisecond, func(context.Context) error {
			if calls.Add(1) >= 3 {
				cancel()
			}
			return nil
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Loop returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Loop did not stop after cancel")
	}
	if calls.Load() < 3 {
		t.Fatalf("expected >=3 calls, got %d", calls.Load())
	}
}

func TestLoopContinuesAfterPollError(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Loop(ctx, 10*time.Millisecond, func(context.Context) error {
			n := calls.Add(1)
			if n >= 3 {
				cancel()
			}
			return errors.New("transient") // every poll errors
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Loop did not stop after cancel")
	}
	if calls.Load() < 3 {
		t.Fatalf("Loop should keep ticking despite errors, got %d calls", calls.Load())
	}
}
