package observability

import (
	"strings"
	"testing"
	"time"
)

func TestCollectorRecordsBoundedOutcomesAndHistograms(t *testing.T) {
	c := New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), true)
	c.Record("/v1/chat/completions", "completed", 2*time.Second, 500*time.Millisecond, true, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s := c.Snapshot(time.Date(2026, 1, 1, 0, 4, 0, 0, time.UTC))
	if s.RecentOutcomes["completed"] != 1 || s.RequestDuration.Count != 1 || s.RequestDuration.Sum != 2 || s.QueueWait.Count != 1 || s.QueueWait.Sum != .5 {
		t.Fatalf("snapshot: %+v", s)
	}
	text := c.Prometheus()
	for _, want := range []string{`router_inference_requests_total{endpoint="/v1/chat/completions",outcome="completed"} 1`, `le="+Inf"`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
}

func TestRecentWindowExpiresAfterFiveMinuteBuckets(t *testing.T) {
	c := New(time.Now(), false)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.Record("/v1/embeddings", "rejected", 0, 0, false, now)
	if got := c.Snapshot(now.Add(5 * time.Minute)).RecentOutcomes["rejected"]; got != 0 {
		t.Fatalf("stale outcome count=%d", got)
	}
}

func TestMetricRoutesAndPrometheusTypes(t *testing.T) {
	c := New(time.Now(), true)
	c.Record("/v1/models", "completed", time.Second, 0, false, time.Now())
	c.Record("/v1/messages/count_tokens", "completed", time.Second, 0, false, time.Now())
	text := c.Prometheus()
	for _, forbidden := range []string{"/v1/models", "/v1/messages/count_tokens"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("unexpected route %s in %s", forbidden, text)
		}
	}
	for _, want := range []string{"# TYPE router_inference_requests_total counter", "# TYPE router_inference_request_duration_seconds histogram", "# TYPE router_inference_queue_wait_seconds histogram", `router_inference_request_duration_seconds_bucket{le="+Inf"}`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
}
