package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

var (
	ErrIdentityQueueFull = errors.New("identity queue full")
	ErrGlobalQueueFull   = errors.New("global queue full")
	ErrQueueTimeout      = errors.New("scheduler queue timeout")
	ErrSchedulerDraining = errors.New("scheduler draining")
	ErrSchedulerPaused   = errors.New("scheduler paused")
)

type Policy struct {
	Weight                            float64
	MaxQueued, MaxActiveWhenContended int
}
type Config struct {
	Capacity, DefaultMaxQueued, DefaultMaxActiveWhenContended, GlobalMaxQueued int
	DefaultWeight                                                              float64
	MaxWait                                                                    time.Duration
	WorkConserving                                                             bool
	DisableWorkConserving                                                      bool
	CancellationGrace                                                          time.Duration
}
type Request struct {
	ID, IdentityID, ModelID, Endpoint string
	Weight                            float64
	MaxQueued, MaxActiveWhenContended int
	EnqueuedAt                        time.Time
}
type RequestInfo struct {
	ID, IdentityID, ModelID, Endpoint, State string
	EnqueuedAt                               time.Time
}
type IdentitySnapshot struct {
	Queued, Active int
	Weight         float64
}
type Snapshot struct {
	Capacity, Active, Reservations, Queued, Available int
	Identities                                        map[string]IdentitySnapshot
	Requests                                          []RequestInfo
	DispatchTotal                                     uint64
}

type queued struct {
	req   Request
	ready chan *Lease
}
type identity struct {
	queue                        []*queued
	active, maxQueued, maxActive int
	weight, credit               float64
}
type Scheduler struct {
	mu                           sync.Mutex
	cfg                          Config
	ids                          map[string]*identity
	order                        []string
	cursor                       int
	active, reservations, queued int
	dispatches                   uint64
	paused, draining             bool
	changed                      chan struct{}
	watchers                     map[chan struct{}]struct{}
}
type Lease struct {
	s                    *Scheduler
	identity             string
	once                 sync.Once
	dispatched, released bool
}

func New(c Config) (*Scheduler, error) {
	if c.DefaultWeight <= 0 {
		c.DefaultWeight = 1
	}
	if !c.DisableWorkConserving {
		c.WorkConserving = true
	}
	if err := validateConfig(c); err != nil {
		return nil, err
	}
	return &Scheduler{cfg: c, ids: map[string]*identity{}, changed: make(chan struct{}), watchers: map[chan struct{}]struct{}{}}, nil
}
func (s *Scheduler) signal() {
	close(s.changed)
	s.changed = make(chan struct{})
	for ch := range s.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func validateConfig(c Config) error {
	if c.Capacity < 1 || c.Capacity > 64 {
		return fmt.Errorf("capacity must be between 1 and 64")
	}
	if math.IsNaN(c.DefaultWeight) || math.IsInf(c.DefaultWeight, 0) || c.DefaultWeight < .1 || c.DefaultWeight > 100 {
		return fmt.Errorf("default weight must be between 0.1 and 100")
	}
	if c.DefaultMaxQueued < 0 || c.DefaultMaxQueued > 10000 || c.GlobalMaxQueued < 0 || c.GlobalMaxQueued > 10000 {
		return fmt.Errorf("queue limits must be between 0 and 10000")
	}
	if c.DefaultMaxActiveWhenContended < 0 || c.DefaultMaxActiveWhenContended > c.Capacity {
		return fmt.Errorf("default active limit must be between 0 and capacity")
	}
	if c.MaxWait < 0 || c.MaxWait > 24*time.Hour {
		return fmt.Errorf("max wait must be between 0 and 24 hours")
	}
	if c.CancellationGrace < 0 || c.CancellationGrace > 5*time.Minute {
		return fmt.Errorf("cancellation grace must be between 0 and 5 minutes")
	}
	return nil
}

// UpdateConfig atomically replaces the complete effective configuration. Existing leases are never preempted.
func (s *Scheduler) UpdateConfig(c Config) error {
	if err := validateConfig(c); err != nil {
		return err
	}
	if c.MaxWait < time.Second || c.CancellationGrace < time.Second {
		return fmt.Errorf("max wait and cancellation grace must be at least 1 second")
	}
	s.mu.Lock()
	s.cfg = c
	s.dispatchLocked()
	s.signal()
	s.mu.Unlock()
	return nil
}

// WatchChanges subscribes to coalesced change notifications. Call cancel to unregister safely.
func (s *Scheduler) WatchChanges() (<-chan struct{}, func()) {
	s.mu.Lock()
	ch := make(chan struct{}, 1)
	s.watchers[ch] = struct{}{}
	s.mu.Unlock()
	var once sync.Once
	return ch, func() { once.Do(func() { s.mu.Lock(); delete(s.watchers, ch); close(ch); s.mu.Unlock() }) }
}
func (s *Scheduler) Submit(ctx context.Context, r Request) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.IdentityID == "" {
		return nil, errors.New("identity ID required")
	}
	if r.EnqueuedAt.IsZero() {
		r.EnqueuedAt = time.Now()
	}
	q := &queued{req: r, ready: make(chan *Lease, 1)}
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		return nil, ErrSchedulerDraining
	}
	id := s.ids[r.IdentityID]
	if id == nil {
		id = &identity{weight: s.cfg.DefaultWeight, maxQueued: s.cfg.DefaultMaxQueued, maxActive: s.cfg.DefaultMaxActiveWhenContended}
		s.ids[r.IdentityID] = id
		s.order = append(s.order, r.IdentityID)
	}
	if id.weight <= 0 {
		id.weight = s.cfg.DefaultWeight
	}
	if r.Weight > 0 {
		id.weight = r.Weight
	}
	if r.MaxQueued > 0 {
		id.maxQueued = r.MaxQueued
	}
	if r.MaxActiveWhenContended > 0 {
		id.maxActive = r.MaxActiveWhenContended
	}
	if id.maxQueued > 0 && len(id.queue) >= id.maxQueued {
		s.mu.Unlock()
		return nil, ErrIdentityQueueFull
	}
	if s.cfg.GlobalMaxQueued > 0 && s.queued >= s.cfg.GlobalMaxQueued {
		s.mu.Unlock()
		return nil, ErrGlobalQueueFull
	}
	id.queue = append(id.queue, q)
	s.queued++
	s.dispatchLocked()
	s.signal()
	maxWait := s.cfg.MaxWait
	s.mu.Unlock()
	var timer <-chan time.Time
	var t *time.Timer
	if maxWait > 0 {
		t = time.NewTimer(maxWait)
		defer t.Stop()
		timer = t.C
	}
	select {
	case l := <-q.ready:
		if l == nil {
			return nil, context.Canceled
		}
		return l, nil
	case <-ctx.Done():
		s.cancelQueued(q)
		return nil, ctx.Err()
	case <-timer:
		s.cancelQueued(q)
		return nil, ErrQueueTimeout
	}
}
func (s *Scheduler) cancelQueued(q *queued) {
	s.mu.Lock()
	id := s.ids[q.req.IdentityID]
	if id == nil {
		s.mu.Unlock()
		return
	}
	for i, x := range id.queue {
		if x == q {
			id.queue = append(id.queue[:i], id.queue[i+1:]...)
			s.queued--
			s.signal()
			s.dispatchLocked()
			s.mu.Unlock()
			return
		}
	}
	// Dispatch won the race and buffered the lease before cancellation was observed.
	s.mu.Unlock()
	select {
	case l := <-q.ready:
		if l != nil {
			l.Release()
		}
	default:
	}
}
func (s *Scheduler) dispatchLocked() {
	if s.paused {
		return
	}
	for s.active+s.reservations < s.cfg.Capacity {
		pending := 0
		for _, key := range s.order {
			if len(s.ids[key].queue) > 0 {
				pending++
			} else {
				s.ids[key].credit = 0
			}
		}
		contended := pending > 1 || !s.cfg.WorkConserving
		chosen := ""
		best := -1.0
		// Start tie-breaking at the next identity, so equal credits cannot pin to insertion order.
		for offset := 0; offset < len(s.order); offset++ {
			key := s.order[(s.cursor+offset)%len(s.order)]
			id := s.ids[key]
			if len(id.queue) == 0 || (contended && id.maxActive > 0 && id.active >= id.maxActive) {
				continue
			}
			if id.weight <= 0 {
				id.weight = s.cfg.DefaultWeight
			}
			id.credit += id.weight
			if id.credit > best {
				best = id.credit
				chosen = key
			}
		}
		if chosen == "" {
			return
		}
		for i, key := range s.order {
			if key == chosen {
				s.cursor = (i + 1) % len(s.order)
				break
			}
		}
		totalWeight := s.totalEligibleWeightLocked()
		s.ids[chosen].credit -= totalWeight
		id := s.ids[chosen]
		q := id.queue[0]
		id.queue = id.queue[1:]
		s.queued--
		s.reservations++
		id.active++
		l := &Lease{s: s, identity: chosen}
		q.ready <- l
		s.dispatches++
	}
}
func (s *Scheduler) totalEligibleWeightLocked() float64 {
	n := 0.0
	for _, k := range s.order {
		id := s.ids[k]
		if len(id.queue) > 0 && (id.maxActive <= 0 || id.active < id.maxActive) {
			n += s.ids[k].weight
		}
	}
	if n == 0 {
		return 1
	}
	return n
}
func (l *Lease) MarkDispatched() {
	if l == nil || l.s == nil {
		return
	}
	s := l.s
	s.mu.Lock()
	if !l.released && !l.dispatched {
		l.dispatched = true
		s.reservations--
		s.active++
		s.signal()
	}
	s.mu.Unlock()
}
func (l *Lease) Release() {
	if l == nil || l.s == nil {
		return
	}
	l.once.Do(func() {
		s := l.s
		s.mu.Lock()
		l.released = true
		id := s.ids[l.identity]
		if l.dispatched {
			s.active--
		} else {
			s.reservations--
		}
		id.active--
		s.dispatchLocked()
		s.signal()
		s.mu.Unlock()
	})
}
func (l *Lease) ReleaseAfter(d time.Duration) {
	if d <= 0 {
		l.Release()
		return
	}
	time.AfterFunc(d, l.Release)
}
func (s *Scheduler) Pause() { s.mu.Lock(); s.paused = true; s.signal(); s.mu.Unlock() }
func (s *Scheduler) Resume() {
	s.mu.Lock()
	s.paused = false
	s.dispatchLocked()
	s.signal()
	s.mu.Unlock()
}
func (s *Scheduler) Drain(ctx context.Context) error {
	s.mu.Lock()
	s.draining = true
	s.paused = false
	s.dispatchLocked()
	s.signal()
	s.mu.Unlock()
	for {
		s.mu.Lock()
		done := s.active+s.reservations+s.queued == 0
		ch := s.changed
		s.mu.Unlock()
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}
func (s *Scheduler) UpdateIdentityPolicy(key string, p Policy) {
	s.mu.Lock()
	id := s.ids[key]
	if id == nil {
		id = &identity{}
		s.ids[key] = id
		s.order = append(s.order, key)
	}
	if p.Weight > 0 {
		id.weight = p.Weight
	}
	id.maxQueued = p.MaxQueued
	id.maxActive = p.MaxActiveWhenContended
	s.dispatchLocked()
	s.signal()
	s.mu.Unlock()
}
func (s *Scheduler) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Snapshot{Capacity: s.cfg.Capacity, Active: s.active, Reservations: s.reservations, Queued: s.queued, Available: s.cfg.Capacity - s.active - s.reservations, Identities: map[string]IdentitySnapshot{}, DispatchTotal: s.dispatches}
	for key, id := range s.ids {
		out.Identities[key] = IdentitySnapshot{Queued: len(id.queue), Active: id.active, Weight: id.weight}
		for _, q := range id.queue {
			out.Requests = append(out.Requests, RequestInfo{ID: q.req.ID, IdentityID: key, ModelID: q.req.ModelID, Endpoint: q.req.Endpoint, State: "queued", EnqueuedAt: q.req.EnqueuedAt})
		}
	}
	return out
}
