package observability

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

var Endpoints = []string{"/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/messages", "/v1/embeddings"}
var Outcomes = []string{"completed", "rejected", "timed_out", "cancelled", "upstream_error", "interrupted"}
var bounds = []float64{.01, .05, .1, .5, 1, 2.5, 5, 10, 30, 60}

type Histogram struct {
	Count uint64  `json:"count"`
	Sum   float64 `json:"sum"`
}
type Snapshot struct {
	ProcessStartedAt      time.Time         `json:"process_started_at"`
	GatewayEnabled        bool              `json:"gateway_enabled"`
	RecentWindowStartedAt time.Time         `json:"recent_window_started_at"`
	RecentWindowEndedAt   time.Time         `json:"recent_window_ended_at"`
	RecentOutcomes        map[string]uint64 `json:"recent_outcomes"`
	RequestDuration       Histogram         `json:"request_duration_seconds"`
	QueueWait             Histogram         `json:"queue_wait_seconds"`
}
type histogram struct {
	Histogram
	buckets [10]uint64
}
type minute struct {
	start  time.Time
	counts [6]uint64
}
type Collector struct {
	mu              sync.Mutex
	started         time.Time
	enabled         bool
	outcomes        [6][6]uint64
	minutes         [5]minute
	duration, queue histogram
}

func New(start time.Time, enabled bool) *Collector {
	if start.IsZero() {
		start = time.Now()
	}
	return &Collector{started: start.UTC(), enabled: enabled}
}
func index(values []string, v string) int {
	for i, x := range values {
		if x == v {
			return i
		}
	}
	return -1
}
func (c *Collector) Record(endpoint, outcome string, duration, queue time.Duration, queued bool, at time.Time) {
	ei, oi := index(Endpoints, endpoint), index(Outcomes, outcome)
	if ei < 0 || oi < 0 {
		return
	}
	if duration < 0 {
		duration = 0
	}
	if queue < 0 {
		queue = 0
	}
	at = at.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outcomes[ei][oi]++
	m := at.Truncate(time.Minute)
	slot := int(m.Unix() / 60 % 5)
	if c.minutes[slot].start != m {
		c.minutes[slot] = minute{start: m}
	}
	c.minutes[slot].counts[oi]++
	observe := func(h *histogram, v float64) {
		h.Count++
		h.Sum += v
		for i, b := range bounds {
			if v <= b {
				h.buckets[i]++
			}
		}
	}
	observe(&c.duration, duration.Seconds())
	if queued {
		observe(&c.queue, queue.Seconds())
	}
}
func (c *Collector) Snapshot(now time.Time) Snapshot {
	if now.IsZero() {
		now = time.Now()
	}
	end := now.UTC().Truncate(time.Minute)
	start := end.Add(-4 * time.Minute)
	c.mu.Lock()
	defer c.mu.Unlock()
	out := Snapshot{ProcessStartedAt: c.started, GatewayEnabled: c.enabled, RecentWindowStartedAt: start, RecentWindowEndedAt: end.Add(time.Minute).Add(-time.Nanosecond), RecentOutcomes: map[string]uint64{}, RequestDuration: c.duration.Histogram, QueueWait: c.queue.Histogram}
	for _, o := range Outcomes {
		out.RecentOutcomes[o] = 0
	}
	for _, m := range c.minutes {
		if !m.start.Before(start) && !m.start.After(end) {
			for i, o := range Outcomes {
				out.RecentOutcomes[o] += m.counts[i]
			}
		}
	}
	return out
}
func (c *Collector) Prometheus() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out strings.Builder
	out.WriteString("# TYPE router_inference_requests_total counter\n")
	for oi, o := range Outcomes {
		for ei, e := range Endpoints {
			fmt.Fprintf(&out, "router_inference_requests_total{endpoint=%q,outcome=%q} %d\n", e, o, c.outcomes[ei][oi])
		}
	}
	writeHist := func(name string, h *histogram) {
		fmt.Fprintf(&out, "# TYPE %s histogram\n", name)
		for i, b := range bounds {
			fmt.Fprintf(&out, "%s_bucket{le=%q} %d\n", name, fmt.Sprint(b), h.buckets[i])
		}
		fmt.Fprintf(&out, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %g\n%s_count %d\n", name, h.Count, name, h.Sum, name, h.Count)
	}
	writeHist("router_inference_request_duration_seconds", &c.duration)
	writeHist("router_inference_queue_wait_seconds", &c.queue)
	return out.String()
}
