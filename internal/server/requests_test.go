package server

import (
	"context"
	"sync"
	"testing"
)

func TestRequestRegistryLifecycleAndCancel(t *testing.T) {
	r := newRequestRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !r.register(liveRequest{ID: "id", UserID: "user", SchedulingID: "identity", Endpoint: "/v1/chat/completions", cancel: cancel}) {
		t.Fatal("register failed")
	}
	if !r.activate("id") {
		t.Fatal("activate failed")
	}
	if !r.cancelRequest("id") {
		t.Fatal("cancel failed")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("context error = %v", ctx.Err())
	}
	r.finish("id")
	if r.cancelRequest("id") {
		t.Fatal("finished request remained registered")
	}
}

func TestCancelUserRequests(t *testing.T) {
	r := newRequestRegistry()
	a, cancelA := context.WithCancel(context.Background())
	b, cancelB := context.WithCancel(context.Background())
	defer cancelA()
	defer cancelB()
	r.register(liveRequest{ID: "a", UserID: "u", cancel: cancelA})
	r.register(liveRequest{ID: "b", UserID: "v", cancel: cancelB})
	r.disableUser("u")
	if a.Err() != context.Canceled || b.Err() != nil {
		t.Fatalf("user cancellation: a=%v b=%v", a.Err(), b.Err())
	}
}

func TestCancelUserRequestOwnerAndLiveState(t *testing.T) {
	for _, state := range []string{"queued", "active"} {
		t.Run(state, func(t *testing.T) {
			r := newRequestRegistry()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.register(liveRequest{ID: "id", UserID: "owner", cancel: cancel})
			if state == "active" && !r.activate("id") {
				t.Fatal("activate failed")
			}
			if r.cancelUserRequest("other", "id") {
				t.Fatal("cross-owner cancellation succeeded")
			}
			if ctx.Err() != nil {
				t.Fatal("cross-owner cancellation had an effect")
			}
			if !r.cancelUserRequest("owner", "id") || ctx.Err() != context.Canceled {
				t.Fatal("owner could not cancel live request")
			}
		})
	}
}

func TestCancelUserRequestMissingFinishedAndIneligible(t *testing.T) {
	r := newRequestRegistry()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	if r.cancelUserRequest("u", "missing") {
		t.Fatal("canceled missing request")
	}
	r.register(liveRequest{ID: "done", UserID: "u", cancel: cancel})
	r.finish("done")
	if r.cancelUserRequest("u", "done") {
		t.Fatal("canceled finished request")
	}
	r.register(liveRequest{ID: "bad", UserID: "u", cancel: cancel})
	r.mu.Lock()
	r.entries["bad"].State = "finished"
	r.mu.Unlock()
	if r.cancelUserRequest("u", "bad") {
		t.Fatal("canceled ineligible request")
	}
}

func TestCancelUserRequestConcurrentFinish(t *testing.T) {
	r := newRequestRegistry()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 100; i++ {
		id := "race"
		r.register(liveRequest{ID: id, UserID: "u", cancel: cancel})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); r.cancelUserRequest("u", id) }()
		go func() { defer wg.Done(); r.finish(id) }()
		wg.Wait()
		if r.cancelUserRequest("u", id) {
			t.Fatal("finished entry was resurrected")
		}
	}
}

func TestRegisterIfRejectsInactiveUser(t *testing.T) {
	r := newRequestRegistry()
	if r.registerIf(liveRequest{ID: "inactive", UserID: "u", cancel: func() {}}, func() bool { return false }) {
		t.Fatal("registered request for inactive user")
	}
}

func TestRegisterIfAndDisableSerializeThenCancel(t *testing.T) {
	r := newRequestRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checking, allowCheck := make(chan struct{}), make(chan struct{})
	registered := make(chan bool, 1)
	go func() {
		registered <- r.registerIf(liveRequest{ID: "early", UserID: "u", cancel: cancel}, func() bool {
			close(checking)
			<-allowCheck
			return true
		})
	}()
	<-checking
	disabled := make(chan struct{})
	go func() {
		r.disableUser("u")
		close(disabled)
	}()
	select {
	case <-disabled:
		t.Fatal("disable finished while registration held the registry lock")
	default:
	}
	close(allowCheck)
	if !<-registered {
		t.Fatal("active user registration failed")
	}
	<-disabled
	if ctx.Err() != context.Canceled {
		t.Fatalf("registered request was not canceled: %v", ctx.Err())
	}
}

func TestRequestRegistryConcurrentFinishCancel(t *testing.T) {
	r := newRequestRegistry()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.register(liveRequest{ID: "id", UserID: "u", cancel: cancel})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				r.finish("id")
			} else {
				r.cancelRequest("id")
			}
		}(i)
	}
	wg.Wait()
	r.finish("id")
	if r.cancelRequest("id") {
		t.Fatal("entry survived concurrent completion")
	}
}
