package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type serverHarness struct {
	server   *Server
	launches atomic.Int64
	closes   atomic.Int64
	mu       sync.Mutex
	proxies  []map[string]any
}

func testServer(t *testing.T) *serverHarness {
	t.Helper()
	harness := &serverHarness{server: NewServer(solverConfig())}
	harness.server.cfg.Version = "3.5.2"
	harness.server.launch = func(ctx context.Context, cfg Config, proxy map[string]any, ua string) (*Browser, error) {
		id := harness.launches.Add(1)
		harness.mu.Lock()
		harness.proxies = append(harness.proxies, proxy)
		harness.mu.Unlock()
		browser := newBrowserClient("http://webdriver.invalid", fmt.Sprint(id))
		browser.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == "DELETE" {
				harness.closes.Add(1)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"value":null}`))}, nil
		})}
		return browser, nil
	}
	harness.server.solve = func(ctx context.Context, browser *Browser, req Request, cfg Config) (map[string]any, string, error) {
		return map[string]any{"browser": browser.SessionID}, "Challenge not detected!", nil
	}
	t.Cleanup(harness.server.Close)
	return harness
}
func apiRequest(t *testing.T, server *Server, method, path, body, contentType string) (int, map[string]any, http.Header) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	result := map[string]any{}
	if method != "HEAD" {
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatalf("invalid JSON %q: %v", response.Body.String(), err)
		}
	} else if response.Body.Len() != 0 {
		t.Fatal("HEAD response has body")
	}
	return response.Code, result, response.Header()
}

func TestHTTPValidationContract(t *testing.T) {
	tests := []struct{ body, message string }{
		{`{}`, "Request parameter 'cmd' is mandatory."},
		{`{"cmd":null}`, "Request parameter 'cmd' is mandatory."},
		{`{"cmd":"unknown"}`, "Request parameter 'cmd' = 'unknown' is invalid."},
		{`{"cmd":false}`, "Request parameter 'cmd' = 'False' is invalid."},
		{`{"cmd":"request.get"}`, "Request parameter 'url' is mandatory in 'request.get' command."},
		{`{"cmd":"request.get","url":null}`, "Request parameter 'url' is mandatory in 'request.get' command."},
		{`{"cmd":"request.get","url":"x","postData":""}`, "Cannot use 'postBody' when sending a GET request."},
		{`{"cmd":"request.post"}`, "Request parameter 'postData' is mandatory in 'request.post' command."},
		{`{"cmd":"sessions.destroy","session":"missing"}`, "The session doesn't exist."},
		{`{"cmd":"sessions.list","maxTimeout":"abc"}`, "invalid literal for int() with base 10: 'abc'"},
		{`{"cmd":"sessions.list","maxTimeout":[]}`, "int() argument must be a string, a bytes-like object or a real number, not 'list'"},
	}
	for _, test := range tests {
		t.Run(test.body, func(t *testing.T) {
			h := testServer(t)
			status, body, _ := apiRequest(t, h.server, "POST", "/v1", test.body, "application/json")
			if status != 500 || body["status"] != "error" || body["message"] != "Error: "+test.message {
				t.Fatalf("status=%d body=%#v", status, body)
			}
			if body["version"] != "3.5.2" || body["startTimestamp"].(float64) > body["endTimestamp"].(float64) {
				t.Fatal(body)
			}
			if _, hidden := body["__error_500__"]; hidden {
				t.Fatal("hidden field leaked")
			}
			if h.launches.Load() != 0 {
				t.Fatal("validation launched browser")
			}
		})
	}
}

func TestRoutingAndBodyHandling(t *testing.T) {
	h := testServer(t)
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/", 200}, {"HEAD", "/", 200}, {"GET", "/health", 200}, {"HEAD", "/health", 200},
		{"GET", "/missing", 404}, {"GET", "/v1", 405}, {"POST", "/health", 405}, {"POST", "/", 405},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			status, body, header := apiRequest(t, h.server, test.method, test.path, "", "")
			if status != test.status {
				t.Fatal(status, body)
			}
			if header.Get("Content-Type") != "application/json" {
				t.Fatal(header)
			}
		})
	}
	_, body, _ := apiRequest(t, h.server, "GET", "/", "", "")
	if !reflect.DeepEqual(body, map[string]any{"msg": "BlazeSolver is ready!", "version": "3.5.2", "userAgent": "Chrome/Test"}) {
		t.Fatal(body)
	}
	for _, input := range []string{"", "null", "false", "[]", "{}"} {
		status, body, _ := apiRequest(t, h.server, "POST", "/v1", input, "application/json")
		if status != 500 || body["message"] != "Error: Request parameter 'cmd' is mandatory." {
			t.Fatal(input, status, body)
		}
	}
	status, body, _ := apiRequest(t, h.server, "POST", "/v1", `{"cmd":"sessions.list"}`, "text/plain")
	if status != 500 || body["message"] != "Error: Request parameter 'cmd' is mandatory." {
		t.Fatal(status, body)
	}
	for _, input := range []string{`{`, `{} {}`, " \n\t "} {
		status, body, _ := apiRequest(t, h.server, "POST", "/v1", input, "application/json")
		if status != 500 || !reflect.DeepEqual(body, map[string]any{"error": "(400, 'Invalid JSON')"}) {
			t.Fatal(status, body)
		}
	}
	status, body, _ = apiRequest(t, h.server, "POST", "/v1", `{"cmd":"sessions.list","unknown":123}`, "application/json; charset=utf-8")
	if status != 200 || body["message"] != "" || !reflect.DeepEqual(body["sessions"], []any{}) {
		t.Fatal(status, body)
	}
}

func TestTimeoutDefaultsAndPythonIntegerConversion(t *testing.T) {
	for _, test := range []struct {
		value        any
		milliseconds int64
	}{
		{nil, 60000}, {float64(0), 60000}, {float64(-1), 60000}, {float64(123.9), 123}, {"123", 123}, {true, 1}, {false, 60000}, {" 1_000 ", 1000},
	} {
		t.Run(fmt.Sprint(test.value), func(t *testing.T) {
			h := testServer(t)
			h.server.solve = func(ctx context.Context, browser *Browser, req Request, cfg Config) (map[string]any, string, error) {
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Fatal("missing deadline")
				}
				remaining := time.Until(deadline)
				expected := time.Duration(test.milliseconds) * time.Millisecond
				if remaining > expected || remaining < expected-100*time.Millisecond {
					t.Fatalf("deadline %s, expected %s", remaining, expected)
				}
				return map[string]any{}, "Challenge not detected!", nil
			}
			_, err := h.server.dispatch(context.Background(), Request{"cmd": "request.get", "url": "https://example.com/", "maxTimeout": test.value})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTimeoutAndBrowserErrorMessages(t *testing.T) {
	h := testServer(t)
	h.server.solve = func(ctx context.Context, browser *Browser, req Request, cfg Config) (map[string]any, string, error) {
		<-ctx.Done()
		return nil, "", ctx.Err()
	}
	_, err := h.server.dispatch(context.Background(), Request{"cmd": "request.get", "url": "x", "maxTimeout": 1})
	if err == nil || err.Error() != "Error solving the challenge. Timeout after 0.001 seconds." {
		t.Fatal(err)
	}
	if h.closes.Load() != 1 {
		t.Fatal("timed out ephemeral browser not closed")
	}
	h.server.solve = func(context.Context, *Browser, Request, Config) (map[string]any, string, error) {
		return nil, "", errors.New("line one\nline two")
	}
	_, err = h.server.dispatch(context.Background(), Request{"cmd": "request.get", "url": "x"})
	if err == nil || err.Error() != `Error solving the challenge. line one\nline two` {
		t.Fatal(err)
	}
}

func TestSessionIdempotencyOrderAndLifecycle(t *testing.T) {
	h := testServer(t)
	command := func(req Request) map[string]any {
		t.Helper()
		result, err := h.server.dispatch(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, id := range []string{"first", "second"} {
		result := command(Request{"cmd": "sessions.create", "session": id})
		if result["message"] != "Session created successfully." {
			t.Fatal(result)
		}
	}
	result := command(Request{"cmd": "sessions.create", "session": "first", "proxy": map[string]any{"url": "ignored"}})
	if result["message"] != "Session already exists." || h.launches.Load() != 2 {
		t.Fatal(result)
	}
	if !reflect.DeepEqual(command(Request{"cmd": "sessions.list"})["sessions"], []any{"first", "second"}) {
		t.Fatal("insertion order lost")
	}
	command(Request{"cmd": "sessions.destroy", "session": "first"})
	if h.closes.Load() != 1 {
		t.Fatal("destroy did not close browser")
	}
	command(Request{"cmd": "sessions.create", "session": "first"})
	if !reflect.DeepEqual(command(Request{"cmd": "sessions.list"})["sessions"], []any{"second", "first"}) {
		t.Fatal("recreated session ordering differs")
	}
	generated := command(Request{"cmd": "sessions.create"})["session"].(string)
	if len(generated) != 36 || generated[14] != '1' {
		t.Fatalf("expected UUID v1 compatible ID: %q", generated)
	}
	h.server.Close()
	if h.closes.Load() != h.launches.Load() {
		t.Fatalf("leaked sessions: launched=%d closed=%d", h.launches.Load(), h.closes.Load())
	}
}

func TestEphemeralRequestsDoNotReuseBrowser(t *testing.T) {
	h := testServer(t)
	var ids []any
	for i := 0; i < 2; i++ {
		result, err := h.server.dispatch(context.Background(), getRequest())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, result["solution"].(map[string]any)["browser"])
	}
	if ids[0] == ids[1] || h.launches.Load() != 2 || h.closes.Load() != 2 {
		t.Fatalf("requests shared browser: %v", ids)
	}
	if len(h.server.sessions.ids()) != 0 {
		t.Fatal("ephemeral request created persistent session")
	}
}

func TestSessionProxyAndTTLCompatibility(t *testing.T) {
	h := testServer(t)
	proxy := map[string]any{"url": "http://proxy.example:8888"}
	_, err := h.server.dispatch(context.Background(), Request{"cmd": "sessions.create", "session": "saved", "proxy": proxy})
	if err != nil {
		t.Fatal(err)
	}
	req := getRequest()
	req["session"] = "saved"
	req["proxy"] = map[string]any{"url": "ignored"}
	if _, err = h.server.dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if h.launches.Load() != 1 {
		t.Fatal("session request relaunched browser")
	}
	entry := h.server.sessions.entries[sessionKey("saved")]
	entry.created = time.Now().Add(-2 * time.Minute)
	req["session_ttl_minutes"] = float64(1)
	if _, err = h.server.dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if h.launches.Load() != 2 || h.closes.Load() != 1 {
		t.Fatal("expired session not replaced")
	}
	if !reflect.DeepEqual(h.proxies[0], proxy) || h.proxies[1] != nil {
		t.Fatalf("TTL recreation must retain original proxy-drop behavior: %#v", h.proxies)
	}
	req["session"] = "implicitly-created"
	delete(req, "session_ttl_minutes")
	if _, err = h.server.dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if h.proxies[2] != nil {
		t.Fatal("request proxy must be ignored when auto-creating named session")
	}
}

func TestEnvironmentProxyFallback(t *testing.T) {
	h := testServer(t)
	h.server.cfg.Proxy = map[string]any{"url": "http://environment:8888"}
	for _, proxyJSON := range []string{"null", `{}`, `{"url":"http://request:8888"}`} {
		status, body, _ := apiRequest(t, h.server, "POST", "/v1", `{"cmd":"request.get","url":"x","proxy":`+proxyJSON+`}`, "application/json")
		if status != 200 {
			t.Fatal(status, body)
		}
	}
	if !reflect.DeepEqual(h.proxies, []map[string]any{{"url": "http://environment:8888"}, {"url": "http://environment:8888"}, {"url": "http://request:8888"}}) {
		t.Fatal(h.proxies)
	}
}

func TestConcurrentSessionCreationAndUse(t *testing.T) {
	h := testServer(t)
	var workers sync.WaitGroup
	failures := make(chan error, 20)
	for i := 0; i < 20; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := h.server.dispatch(context.Background(), Request{"cmd": "sessions.create", "session": "shared"})
			failures <- err
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if h.launches.Load() != 1 {
		t.Fatalf("racing creates launched %d browsers", h.launches.Load())
	}
	var active, peak atomic.Int64
	h.server.solve = func(ctx context.Context, browser *Browser, req Request, cfg Config) (map[string]any, string, error) {
		current := active.Add(1)
		for current > peak.Load() {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
		return map[string]any{}, "Challenge not detected!", nil
	}
	failures = make(chan error, 20)
	for i := 0; i < 20; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			req := getRequest()
			req["session"] = "shared"
			_, err := h.server.dispatch(context.Background(), req)
			failures <- err
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() != 1 {
		t.Fatalf("same browser used concurrently by %d requests", peak.Load())
	}
	if h.closes.Load() != 0 {
		t.Fatal("persistent browser closed after request")
	}
}

func TestDifferentSessionsCanRunConcurrently(t *testing.T) {
	h := testServer(t)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	h.server.solve = func(ctx context.Context, browser *Browser, req Request, cfg Config) (map[string]any, string, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
		return map[string]any{}, "Challenge not detected!", nil
	}
	done := make(chan error, 2)
	for _, id := range []string{"one", "two"} {
		go func(id string) {
			req := getRequest()
			req["session"] = id
			_, err := h.server.dispatch(context.Background(), req)
			done <- err
		}(id)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("different sessions blocked each other")
		}
	}
	unblock()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestInvalidInputsMatchPythonReference(t *testing.T) {
	// These strings follow Python 3.11, the supported Docker reference runtime.
	for _, test := range []struct{ raw, error string }{
		{`[1]`, "cannot convert dictionary update sequence element #0 to a sequence"},
		{`"text"`, "dictionary update sequence element #0 has length 1; 2 is required"},
		{`1`, "argument of type 'int' is not iterable"},
		{`true`, "argument of type 'bool' is not iterable"},
		{`["proxy"]`, "'list' object has no attribute 'get'"},
		{`"proxy"`, "'str' object has no attribute 'get'"},
		{`[[]]`, "dictionary update sequence element #0 has length 0; 2 is required"},
		{`[["a"]]`, "dictionary update sequence element #0 has length 1; 2 is required"},
		{`[["a","b","c"]]`, "dictionary update sequence element #0 has length 3; 2 is required"},
		{`[[[],"value"]]`, "unhashable type: 'list'"},
	} {
		t.Run(test.raw, func(t *testing.T) {
			h := testServer(t)
			status, body, _ := apiRequest(t, h.server, "POST", "/v1", test.raw, "application/json")
			if status != 500 || !reflect.DeepEqual(body, map[string]any{"error": test.error}) {
				t.Fatalf("status=%d body=%#v", status, body)
			}
		})
	}
	h := testServer(t)
	status, body, _ := apiRequest(t, h.server, "POST", "/v1", `[["cmd","sessions.list"]]`, "application/json")
	if status != 200 || body["status"] != "ok" || !reflect.DeepEqual(body["sessions"], []any{}) {
		t.Fatal(status, body)
	}
	status, body, _ = apiRequest(t, h.server, "POST", "/v1", strings.Repeat(" ", 102401), "application/json")
	if status != 500 || !reflect.DeepEqual(body, map[string]any{"error": "(413, 'Request entity too large')"}) {
		t.Fatal(status, body)
	}
	for _, input := range []string{"1__0", "_10", "10_", "+_1"} {
		status, body, _ := apiRequest(t, h.server, "POST", "/v1", `{"cmd":"sessions.list","maxTimeout":"`+input+`"}`, "application/json")
		if status != 500 || body["message"] != "Error: invalid literal for int() with base 10: '"+input+"'" {
			t.Fatal(status, body)
		}
	}
	for _, test := range []struct{ value, kind string }{{`[]`, "list"}, {`{}`, "dict"}} {
		status, body, _ := apiRequest(t, h.server, "POST", "/v1", `{"cmd":"sessions.destroy","session":`+test.value+`}`, "application/json")
		if status != 500 || body["message"] != "Error: unhashable type: '"+test.kind+"'" {
			t.Fatal(status, body)
		}
	}
}

func TestSessionNumericIdentityMatchesPythonDictionary(t *testing.T) {
	h := testServer(t)
	for _, id := range []any{json.Number("1"), true, json.Number("1.0")} {
		result, err := h.server.dispatch(context.Background(), Request{"cmd": "sessions.create", "session": id})
		if err != nil {
			t.Fatal(err)
		}
		if result["session"] != json.Number("1") {
			t.Fatalf("must return original session ID: %#v", result)
		}
	}
	if h.launches.Load() != 1 {
		t.Fatal("Python-equal IDs must address same session")
	}
	_, err := h.server.dispatch(context.Background(), Request{"cmd": "sessions.create", "session": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if h.launches.Load() != 2 {
		t.Fatal("string and numeric session IDs must remain distinct")
	}
}

func TestWaitingForSessionCanBeCanceled(t *testing.T) {
	h := testServer(t)
	entry, _, err := h.server.acquireSession(context.Background(), "busy", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() { _, _, err := h.server.acquireSession(ctx, "busy", nil, 0); waiting <- err }()
	cancel()
	select {
	case err := <-waiting:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		entry.use.Unlock()
		t.Fatal("canceled request remained blocked on session")
	}
	entry.use.Unlock()
	if h.launches.Load() != 1 {
		t.Fatal("canceled waiter launched browser")
	}
}

func TestFailedSessionLaunchCanBeRetriedWithoutOrphaningBrowser(t *testing.T) {
	h := testServer(t)
	original := h.server.launch
	var attempts atomic.Int64
	h.server.launch = func(ctx context.Context, cfg Config, proxy map[string]any, ua string) (*Browser, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("first launch failed")
		}
		return original(ctx, cfg, proxy, ua)
	}
	if _, _, err := h.server.acquireSession(context.Background(), "retry", nil, 0); err == nil {
		t.Fatal("first launch should fail")
	}
	entry, fresh, err := h.server.acquireSession(context.Background(), "retry", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	entry.use.Unlock()
	if !fresh || !reflect.DeepEqual(h.server.sessions.ids(), []any{"retry"}) {
		t.Fatal("successful retry missing from registry")
	}
	h.server.Close()
	if h.launches.Load() != 1 || h.closes.Load() != 1 {
		t.Fatal("retry leaked browser")
	}
}

func TestShutdownWaitsForStartingSessionAndRejectsNewSessions(t *testing.T) {
	h := testServer(t)
	original := h.server.launch
	started := make(chan struct{})
	release := make(chan struct{})
	h.server.launch = func(ctx context.Context, cfg Config, proxy map[string]any, ua string) (*Browser, error) {
		close(started)
		<-release
		return original(ctx, cfg, proxy, ua)
	}
	requestDone := make(chan error, 1)
	go func() {
		_, err := h.server.dispatch(context.Background(), Request{"cmd": "sessions.create", "session": "starting"})
		requestDone <- err
	}()
	<-started
	closed := make(chan struct{})
	go func() { h.server.Close(); close(closed) }()
	deadline := time.Now().Add(time.Second)
	for {
		h.server.sessions.mu.Lock()
		shuttingDown := h.server.sessions.closed
		h.server.sessions.mu.Unlock()
		if shuttingDown {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("shutdown never started")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-closed:
		close(release)
		t.Fatal("shutdown returned before starting browser could be cleaned up")
	default:
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish")
	}
	if h.closes.Load() != 1 {
		t.Fatal("starting session browser leaked during shutdown")
	}
	if _, _, err := h.server.acquireSession(context.Background(), "after-close", nil, 0); err == nil {
		t.Fatal("shutdown server accepted new session")
	}
}

func TestLargeNumericSessionIDsRemainDistinct(t *testing.T) {
	h := testServer(t)
	for _, id := range []json.Number{"9007199254740992", "9007199254740993"} {
		if _, err := h.server.dispatch(context.Background(), Request{"cmd": "sessions.create", "session": id}); err != nil {
			t.Fatal(err)
		}
	}
	if h.launches.Load() != 2 {
		t.Fatal("integer precision loss merged separate sessions")
	}
	if sessionKey(json.Number("-0.0")) != sessionKey(json.Number("0")) {
		t.Fatal("Python treats negative zero as zero")
	}
}
