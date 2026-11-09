package server

import (
	"context"
	"sync"
	"time"
)

const maxLiveRequests = 10064 // maximum supported global queue (10,000) plus capacity (64)

type liveRequest struct {
	ID, UserID, SchedulingID, Endpoint string
	State                              string
	EnqueuedAt, StartedAt              time.Time
	cancel                             context.CancelFunc
}

type requestRegistry struct {
	mu       sync.Mutex
	entries  map[string]*liveRequest
	watchers map[chan struct{}]struct{}
}

func newRequestRegistry() *requestRegistry {
	return &requestRegistry{entries: make(map[string]*liveRequest), watchers: make(map[chan struct{}]struct{})}
}

func (r *requestRegistry) signalLocked() {
	for ch := range r.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// WatchChanges returns a coalesced notification stream; cancel unregisters and closes it safely.
func (r *requestRegistry) WatchChanges() (<-chan struct{}, func()) {
	r.mu.Lock()
	ch := make(chan struct{}, 1)
	r.watchers[ch] = struct{}{}
	r.mu.Unlock()
	var once sync.Once
	return ch, func() { once.Do(func() { r.mu.Lock(); delete(r.watchers, ch); close(ch); r.mu.Unlock() }) }
}

func (r *requestRegistry) register(req liveRequest) bool {
	return r.registerIf(req, nil)
}

func (r *requestRegistry) registerIf(req liveRequest, userStillActive func() bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.UserID != "" && userStillActive != nil && !userStillActive() {
		return false
	}
	if len(r.entries) >= maxLiveRequests {
		return false
	}
	req.State, req.EnqueuedAt = "queued", time.Now()
	r.entries[req.ID] = &req
	r.signalLocked()
	return true
}

func (r *requestRegistry) activate(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entries[id]
	if entry == nil || entry.State != "queued" {
		return false
	}
	entry.State, entry.StartedAt = "active", time.Now()
	r.signalLocked()
	return true
}

func (r *requestRegistry) finish(id string) {
	r.mu.Lock()
	delete(r.entries, id)
	r.signalLocked()
	r.mu.Unlock()
}

func (r *requestRegistry) cancelRequest(id string) bool {
	r.mu.Lock()
	entry := r.entries[id]
	if entry == nil {
		r.mu.Unlock()
		return false
	}
	cancel := entry.cancel
	r.mu.Unlock()
	cancel()
	return true
}

func (r *requestRegistry) cancelUserRequest(userID, requestID string) bool {
	r.mu.Lock()
	entry := r.entries[requestID]
	if userID == "" || requestID == "" || entry == nil || entry.UserID != userID || (entry.State != "queued" && entry.State != "active") {
		r.mu.Unlock()
		return false
	}
	cancel := entry.cancel
	r.mu.Unlock()
	cancel()
	return true
}

func (r *requestRegistry) disableUser(userID string) {
	if userID == "" {
		return
	}
	r.mu.Lock()
	var cancels []context.CancelFunc
	for _, entry := range r.entries {
		if entry.UserID == userID {
			cancels = append(cancels, entry.cancel)
		}
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}
