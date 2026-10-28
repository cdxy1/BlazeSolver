package main

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

type durationMetric struct {
	count   int
	sum     float64
	buckets [4]int
	created int64
}
type counterMetric struct {
	count   int
	created int64
}
type Metrics struct {
	mu        sync.Mutex
	durations map[string]*durationMetric
	counts    map[[2]string]*counterMetric
}

func newMetrics() *Metrics {
	return &Metrics{durations: map[string]*durationMetric{}, counts: map[[2]string]*counterMetric{}}
}

func (m *Metrics) observe(req Request, response map[string]any, seconds float64) {
	domain := "unknown"
	var requestURL any
	if solution, ok := response["solution"].(map[string]any); ok {
		requestURL = solution["url"]
	} else {
		requestURL = req["url"]
	}
	if requestURL != nil {
		if address, err := url.Parse(textValue(requestURL)); err == nil && address.Host != "" {
			domain = address.Host
		}
	}
	result := "unknown"
	switch response["message"] {
	case "Challenge solved!":
		result = "solved"
	case "Challenge not detected!":
		result = "not_detected"
	default:
		if response["status"] == "error" {
			result = "error"
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := [2]string{domain, result}
	if m.counts[key] == nil {
		m.counts[key] = &counterMetric{created: time.Now().Unix()}
	}
	m.counts[key].count++
	if m.durations[domain] == nil {
		m.durations[domain] = &durationMetric{created: time.Now().Unix()}
	}
	duration := m.durations[domain]
	duration.count++
	duration.sum += seconds
	for i, bound := range []float64{0, 10, 25, 50} {
		if seconds <= bound {
			duration.buckets[i]++
		}
	}
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Fprintln(w, "# HELP blazesolver_request_total Total requests with result\n# TYPE blazesolver_request_total counter")
	keys := make([][2]string, 0, len(m.counts))
	for key := range m.counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] == keys[j][0] {
			return keys[i][1] < keys[j][1]
		}
		return keys[i][0] < keys[j][0]
	})
	for _, key := range keys {
		fmt.Fprintf(w, "blazesolver_request_total{domain=%s,result=%s} %d\n", strconv.Quote(key[0]), strconv.Quote(key[1]), m.counts[key].count)
	}
	fmt.Fprintln(w, "# HELP blazesolver_request_created Total requests with result\n# TYPE blazesolver_request_created gauge")
	for _, key := range keys {
		fmt.Fprintf(w, "blazesolver_request_created{domain=%s,result=%s} %d\n", strconv.Quote(key[0]), strconv.Quote(key[1]), m.counts[key].created)
	}
	fmt.Fprintln(w, "# HELP blazesolver_request_duration Request duration in seconds\n# TYPE blazesolver_request_duration histogram")
	domains := make([]string, 0, len(m.durations))
	for domain := range m.durations {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	for _, domain := range domains {
		metric := m.durations[domain]
		for i, bound := range []string{"0.0", "10.0", "25.0", "50.0"} {
			fmt.Fprintf(w, "blazesolver_request_duration_bucket{domain=%s,le=%s} %d\n", strconv.Quote(domain), strconv.Quote(bound), metric.buckets[i])
		}
		fmt.Fprintf(w, "blazesolver_request_duration_bucket{domain=%s,le=\"+Inf\"} %d\nblazesolver_request_duration_count{domain=%s} %d\nblazesolver_request_duration_sum{domain=%s} %g\n", strconv.Quote(domain), metric.count, strconv.Quote(domain), metric.count, strconv.Quote(domain), metric.sum)
	}
	fmt.Fprintln(w, "# HELP blazesolver_request_duration_created Request duration in seconds\n# TYPE blazesolver_request_duration_created gauge")
	for _, domain := range domains {
		fmt.Fprintf(w, "blazesolver_request_duration_created{domain=%s} %d\n", strconv.Quote(domain), m.durations[domain].created)
	}
}
