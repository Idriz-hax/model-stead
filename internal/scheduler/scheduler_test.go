package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCapacityLeaseAndRelease(t *testing.T) {
	s, err := New(Config{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.Submit(context.Background(), Request{ID: "one", IdentityID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot(); got.Active+got.Reservations != 1 {
		t.Fatalf("lease not reserved: %+v", got)
	}
	l.MarkDispatched()
	l.Release()
	l.Release()
	if got := s.Snapshot(); got.Active+got.Reservations != 0 {
		t.Fatalf("lease not released: %+v", got)
	}
}

func TestQueueLimitsAndCancellation(t *testing.T) {
	s, err := New(Config{Capacity: 1, DefaultMaxQueued: 1})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Submit(context.Background(), Request{ID: "first", IdentityID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, e := s.Submit(ctx, Request{ID: "queued", IdentityID: "b"}); done <- e }()
	deadline := time.Now().Add(time.Second)
	for s.Snapshot().Queued == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	first.Release()
}

func TestPauseAndResume(t *testing.T) {
	s, err := New(Config{Capacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	s.Pause()
	done := make(chan *Lease, 1)
	go func() { l, _ := s.Submit(context.Background(), Request{ID: "one", IdentityID: "a"}); done <- l }()
	time.Sleep(10 * time.Millisecond)
	if s.Snapshot().Reservations != 0 {
		t.Fatal("dispatched while paused")
	}
	s.Resume()
	select {
	case l := <-done:
		l.Release()
	case <-time.After(time.Second):
		t.Fatal("did not resume")
	}
}

func TestQueueTimeoutAndDynamicPolicy(t *testing.T) {
	s, err := New(Config{Capacity: 1, MaxWait: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.Submit(context.Background(), Request{ID: "hold", IdentityID: "hold"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Submit(context.Background(), Request{ID: "wait", IdentityID: "wait"}); !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("got %v", err)
	}
	lease.Release()
	s.UpdateIdentityPolicy("policy", Policy{Weight: 3, MaxQueued: 2})
	if got := s.Snapshot().Identities["policy"].Weight; got != 3 {
		t.Fatalf("weight %v", got)
	}
}

func TestWeightedAdmissionEventuallyServesBoth(t *testing.T) {
	s, err := New(Config{Capacity: 1, DefaultWeight: 1})
	if err != nil {
		t.Fatal(err)
	}
	s.UpdateIdentityPolicy("heavy", Policy{Weight: 2})
	// Queue work while paused so the active set is deterministic.
	s.Pause()
	results := make(chan *Lease, 9)
	for _, id := range []string{"heavy", "heavy", "heavy", "heavy", "heavy", "heavy", "light", "light", "light"} {
		id := id
		go func() {
			l, e := s.Submit(context.Background(), Request{IdentityID: id})
			if e == nil {
				results <- l
			}
		}()
	}
	deadline := time.Now().Add(time.Second)
	for s.Snapshot().Queued < 9 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Snapshot().Queued < 9 {
		t.Fatal("timed out queuing weighted requests")
	}
	s.Resume()
	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		select {
		case l := <-results:
			counts[l.identity]++
			l.Release()
		case <-time.After(time.Second):
			t.Fatal("scheduler stalled")
		}
	}
	if counts["heavy"] != 6 || counts["light"] != 3 {
		t.Fatalf("unexpected weighted distribution: %v", counts)
	}
}

func TestFractionalWeightsArePreserved(t *testing.T) {
	s, err := New(Config{Capacity: 1, DefaultWeight: 1})
	if err != nil {
		t.Fatal(err)
	}
	s.UpdateIdentityPolicy("priority", Policy{Weight: 1.5})
	if got := s.Snapshot().Identities["priority"].Weight; got != 1.5 {
		t.Fatalf("fractional weight truncated: %v", got)
	}
}

func TestF01SustainedIdentityFairness(t *testing.T) {
	s, err := New(Config{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	s.Pause()
	type result struct {
		id    string
		lease *Lease
	}
	got := make(chan result, 32)
	for i := 0; i < 30; i++ {
		go func() {
			l, e := s.Submit(context.Background(), Request{IdentityID: "A"})
			if e == nil {
				got <- result{l.identity, l}
			}
		}()
	}
	for i := 0; i < 2; i++ {
		go func() {
			l, e := s.Submit(context.Background(), Request{IdentityID: "B"})
			if e == nil {
				got <- result{l.identity, l}
			}
		}()
	}
	waitQueued(t, s, 32)
	s.Resume()
	counts := map[string]int{}
	firstB := 32
	for i := 0; i < 32; i++ {
		r := <-got
		if r.id == "B" && firstB == 32 {
			firstB = i
		}
		counts[r.id]++
		r.lease.Release()
	}
	if counts["A"] != 30 || counts["B"] != 2 {
		t.Fatalf("lost requests: %v", counts)
	}
	if firstB > 1 {
		t.Fatalf("B starved behind A backlog; first B at %d", firstB)
	}
	if s.Snapshot().Reservations+s.Snapshot().Active != 0 {
		t.Fatal("lease accounting leaked")
	}
}

func waitQueued(t *testing.T, s *Scheduler, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for s.Snapshot().Queued < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Snapshot().Queued < n {
		t.Fatalf("queued %d, want %d", s.Snapshot().Queued, n)
	}
}

func TestContendedActiveCapAndWorkConserving(t *testing.T) {
	s, err := New(Config{Capacity: 4, DefaultMaxActiveWhenContended: 1})
	if err != nil {
		t.Fatal(err)
	}
	// A single identity may fill all capacity even with a contention cap.
	a := make([]*Lease, 0, 4)
	for i := 0; i < 4; i++ {
		l, e := s.Submit(context.Background(), Request{IdentityID: "A"})
		if e != nil {
			t.Fatal(e)
		}
		a = append(a, l)
	}
	if got := s.Snapshot(); got.Reservations != 4 {
		t.Fatalf("single user not work-conserving: %+v", got)
	}
	// Free capacity and create competing queues; each identity is limited to one.
	for _, l := range a {
		l.Release()
	}
	s.Pause()
	result := make(chan *Lease, 8)
	for _, id := range []string{"A", "A", "A", "B", "B", "B"} {
		id := id
		go func() {
			l, e := s.Submit(context.Background(), Request{IdentityID: id})
			if e == nil {
				result <- l
			}
		}()
	}
	waitQueued(t, s, 6)
	s.Resume()
	if snap := s.Snapshot(); snap.Reservations > 2 {
		t.Fatalf("contended cap bypassed: %+v", snap)
	}
	for i := 0; i < 6; i++ {
		select {
		case l := <-result:
			l.Release()
		case <-time.After(time.Second):
			t.Fatal("contended queues stalled")
		}
	}
}

func TestNonWorkConservingHonorsSingleIdentityActiveLimit(t *testing.T) {
	s, err := New(Config{Capacity: 2, DefaultMaxActiveWhenContended: 1, DisableWorkConserving: true})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Submit(context.Background(), Request{IdentityID: "A"})
	if err != nil {
		t.Fatal(err)
	}
	queued := make(chan *Lease, 1)
	go func() {
		lease, _ := s.Submit(context.Background(), Request{IdentityID: "A"})
		queued <- lease
	}()
	waitQueued(t, s, 1)
	if snap := s.Snapshot(); snap.Active+snap.Reservations != 1 {
		t.Fatalf("non-work-conserving scheduler exceeded identity cap: %+v", snap)
	}
	first.Release()
	select {
	case second := <-queued:
		if second == nil {
			t.Fatal("second lease missing")
		}
		second.Release()
	case <-time.After(time.Second):
		t.Fatal("queued request did not dispatch after capacity released")
	}
}

func TestCanceledDeliveredLeaseDoesNotLeak(t *testing.T) {
	for i := 0; i < 100; i++ {
		s, _ := New(Config{Capacity: 1})
		s.Pause()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { _, _ = s.Submit(ctx, Request{IdentityID: "x"}); close(done) }()
		waitQueued(t, s, 1)
		cancel()
		s.Resume()
		<-done
		if snap := s.Snapshot(); snap.Active+snap.Reservations != 0 {
			t.Fatalf("iteration %d leaked lease: %+v", i, snap)
		}
	}
}

func TestConcurrentSubmitCancelAccounting(t *testing.T) {
	s, _ := New(Config{Capacity: 8, GlobalMaxQueued: 1000})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			if i%2 == 0 {
				cancel()
			}
			l, e := s.Submit(ctx, Request{IdentityID: string(rune('a' + i%5))})
			if e == nil {
				l.Release()
			} else if !errors.Is(e, context.Canceled) {
				t.Errorf("submit: %v", e)
			}
		}(i)
	}
	wg.Wait()
	snap := s.Snapshot()
	if snap.Active+snap.Reservations != 0 || snap.Queued != 0 {
		t.Fatalf("accounting: %+v", snap)
	}
}

func TestUpdateConfigValidationAndCapacityReduction(t *testing.T) {
	s, err := New(Config{Capacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Submit(context.Background(), Request{IdentityID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Submit(context.Background(), Request{IdentityID: "b"})
	if err != nil {
		t.Fatal(err)
	}
	before := s.Snapshot()
	bad := Config{Capacity: 65, DefaultWeight: 1, MaxWait: time.Second, CancellationGrace: time.Second}
	if err := s.UpdateConfig(bad); err == nil {
		t.Fatal("invalid config accepted")
	}
	if got := s.Snapshot(); got.Capacity != before.Capacity || got.Reservations != before.Reservations {
		t.Fatalf("invalid update mutated state: %+v", got)
	}
	cfg := Config{Capacity: 1, DefaultWeight: 1, MaxWait: time.Second, CancellationGrace: time.Second}
	if err := s.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot(); got.Reservations != 2 || got.Available != -1 {
		t.Fatalf("capacity reduction preempted leases: %+v", got)
	}
	queued := make(chan *Lease, 1)
	go func() { l, _ := s.Submit(context.Background(), Request{IdentityID: "c"}); queued <- l }()
	waitQueued(t, s, 1)
	a.Release()
	select {
	case l := <-queued:
		l.Release()
		t.Fatal("dispatched while usage equaled reduced capacity")
	case <-time.After(30 * time.Millisecond):
	}
	b.Release()
	select {
	case l := <-queued:
		if l == nil {
			t.Fatal("missing lease")
		}
		l.Release()
	case <-time.After(time.Second):
		t.Fatal("dispatch did not resume below capacity")
	}
}

func TestWatchChangesAndSnapshotIsolation(t *testing.T) {
	s, err := New(Config{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := s.WatchChanges()
	defer cancel()
	s.Pause()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no change notification")
	}
	snap := s.Snapshot()
	snap.Identities["external"] = IdentitySnapshot{}
	if _, ok := s.Snapshot().Identities["external"]; ok {
		t.Fatal("snapshot exposed mutable state")
	}
}

func TestConcurrentUpdateConfigSnapshotAndWatch(t *testing.T) {
	s, err := New(Config{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	changes, cancel := s.WatchChanges()
	defer cancel()

	configs := []Config{
		{Capacity: 1, DefaultWeight: 1, MaxWait: time.Second, CancellationGrace: time.Second},
		{Capacity: 8, DefaultWeight: 2, MaxWait: 2 * time.Second, CancellationGrace: 2 * time.Second},
		{Capacity: 64, DefaultWeight: 3, MaxWait: 3 * time.Second, CancellationGrace: 3 * time.Second},
	}
	done := make(chan struct{})
	var updaters, reader sync.WaitGroup
	for _, cfg := range configs {
		cfg := cfg
		updaters.Add(1)
		go func() {
			defer updaters.Done()
			for i := 0; i < 100; i++ {
				if err := s.UpdateConfig(cfg); err != nil {
					t.Errorf("UpdateConfig: %v", err)
					return
				}
			}
		}()
	}
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-done:
				return
			case <-changes:
			case <-time.After(time.Millisecond):
			}
			snap := s.Snapshot()
			if snap.Capacity < 1 || snap.Capacity > 64 || snap.Available != snap.Capacity-snap.Active-snap.Reservations {
				t.Errorf("invalid snapshot: %+v", snap)
				return
			}
		}
	}()
	updaters.Wait()
	close(done)
	reader.Wait()
}
