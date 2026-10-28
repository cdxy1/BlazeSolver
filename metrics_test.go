package main

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestMetricsKeepNamesLabelsBucketsAndRedirectDomain(t *testing.T) {
	m := newMetrics()
	m.observe(Request{"url": "https://before.test/"}, map[string]any{"message": "Challenge solved!", "solution": map[string]any{"url": "https://after.test:443/page"}}, 12)
	m.observe(Request{"url": "https://before.test/"}, map[string]any{"status": "error", "message": "Error: failed"}, 1)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	for _, line := range []string{
		`blazesolver_request_total{domain="after.test:443",result="solved"} 1`,
		`blazesolver_request_total{domain="before.test",result="error"} 1`,
		`blazesolver_request_duration_bucket{domain="after.test:443",le="10.0"} 0`,
		`blazesolver_request_duration_bucket{domain="after.test:443",le="25.0"} 1`,
		`blazesolver_request_duration_bucket{domain="after.test:443",le="+Inf"} 1`,
		`blazesolver_request_duration_sum{domain="after.test:443"} 12`,
	} {
		if !strings.Contains(w.Body.String(), line) {
			t.Errorf("missing %s in %s", line, w.Body.String())
		}
	}
}

func TestMetricsConcurrentRequests(t *testing.T) {
	m := newMetrics()
	var group sync.WaitGroup
	for i := 0; i < 50; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			m.observe(Request{}, map[string]any{"message": "Challenge not detected!"}, 0.1)
			m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil))
		}()
	}
	group.Wait()
	if m.counts[[2]string{"unknown", "not_detected"}].count != 50 {
		t.Fatal("lost request counts")
	}
}
