package backup

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunScheduledWaitsForTickAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		RunScheduled(ctx, 25*time.Millisecond, func(context.Context) error { calls.Add(1); return nil })
		close(done)
	}()
	time.Sleep(5 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("ran immediately")
	}
	deadline := time.After(time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("did not run on tick")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
}
