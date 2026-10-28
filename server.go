package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var headlessWord = regexp.MustCompile(`(?i)headless`)

type browserFactory func(context.Context, Config, map[string]any, string) (*Browser, error)
type solveFunc func(context.Context, *Browser, Request, Config) (map[string]any, string, error)

type Server struct {
	cfg       Config
	sessions  sessionStore
	launch    browserFactory
	solve     solveFunc
	metrics   *Metrics
	requestID atomic.Uint64
}

func NewServer(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Version == "" {
		cfg.Version = "1.0.0"
	}
	return &Server{cfg: cfg, sessions: sessionStore{entries: make(map[string]*savedSession)}, launch: LaunchBrowser, solve: Solve, metrics: newMetrics()}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	status := http.StatusOK
	var body any
	switch {
	case r.URL.Path == "/" && (r.Method == "GET" || r.Method == "HEAD"):
		body = map[string]any{"msg": "BlazeSolver is ready!", "version": s.cfg.Version, "userAgent": s.cfg.UserAgent}
	case r.URL.Path == "/health" && (r.Method == "GET" || r.Method == "HEAD"):
		body = map[string]any{"status": "ok"}
	case r.URL.Path == "/v1" && r.Method == "POST":
		req, err := decodeRequest(r)
		if err != nil {
			status = 500
			body = map[string]any{"error": err.Error()}
			break
		}
		if !truthy(req["proxy"]) && s.cfg.Proxy != nil {
			req["proxy"] = s.cfg.Proxy
		}
		body, status = s.handleCommand(r.Context(), req)
	case r.URL.Path == "/" || r.URL.Path == "/health" || r.URL.Path == "/v1":
		status = 405
		if r.URL.Path == "/v1" {
			w.Header().Set("Allow", "POST")
		} else {
			w.Header().Set("Allow", "GET, HEAD")
		}
		body = map[string]any{"error": "Method not allowed.", "status_code": status}
	default:
		status = 404
		body = map[string]any{"error": "Not found: " + pythonRepr(r.URL.Path), "status_code": status}
	}
	w.WriteHeader(status)
	if r.Method != "HEAD" {
		encoder := json.NewEncoder(w)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(body)
	}
}

func decodeRequest(r *http.Request) (Request, error) {
	if r.ContentLength == 0 {
		return Request{}, nil
	}
	contentType := strings.ToLower(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	if contentType != "application/json" && contentType != "application/json-rpc" {
		return Request{}, nil
	}
	const maximumJSONBody = 102400
	raw, err := io.ReadAll(io.LimitReader(r.Body, maximumJSONBody+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maximumJSONBody {
		return nil, errors.New("(413, 'Request entity too large')")
	}
	if len(raw) == 0 {
		return Request{}, nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New("(400, 'Invalid JSON')")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("(400, 'Invalid JSON')")
	}
	if !truthy(value) {
		return Request{}, nil
	}
	if object, ok := value.(map[string]any); ok {
		return Request(object), nil
	}
	return requestFromPairs(value)
}

func requestFromPairs(value any) (Request, error) {
	var pairs []any
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			if text, ok := item.(string); ok && text == "proxy" {
				return nil, errors.New("'list' object has no attribute 'get'")
			}
		}
		pairs = value
	case string:
		if strings.Contains(value, "proxy") {
			return nil, errors.New("'str' object has no attribute 'get'")
		}
		for _, char := range value {
			pairs = append(pairs, string(char))
		}
	default:
		return nil, fmt.Errorf("argument of type '%s' is not iterable", pythonType(value))
	}
	request := Request{}
	for index, item := range pairs {
		var pair []any
		switch item := item.(type) {
		case []any:
			pair = item
		case string:
			for _, char := range item {
				pair = append(pair, string(char))
			}
		case map[string]any:
			for key := range item {
				pair = append(pair, key)
			}
		default:
			return nil, fmt.Errorf("cannot convert dictionary update sequence element #%d to a sequence", index)
		}
		if len(pair) != 2 {
			return nil, fmt.Errorf("dictionary update sequence element #%d has length %d; 2 is required", index, len(pair))
		}
		if err := checkSessionID(pair[0]); err != nil {
			return nil, err
		}
		if key, ok := pair[0].(string); ok {
			request[key] = pair[1]
		}
	}
	return request, nil
}

func (s *Server) handleCommand(ctx context.Context, req Request) (map[string]any, int) {
	start := time.Now()
	id := s.requestID.Add(1)
	log := s.cfg.Logger.With("request", id, "cmd", req["cmd"])
	log.Info("Request started", "url", req["url"], "session", req["session"])
	response, err := s.dispatch(ctx, req)
	status := http.StatusOK
	if err != nil {
		response = map[string]any{"status": "error", "message": "Error: " + err.Error()}
		status = 500
		log.Error("Request failed", "error", err)
	}
	response["startTimestamp"], response["endTimestamp"], response["version"] = start.UnixMilli(), time.Now().UnixMilli(), s.cfg.Version
	if s.cfg.PrometheusEnabled {
		s.metrics.observe(req, response, time.Since(start).Seconds())
	}
	log.Info("Request finished", "status", response["status"], "duration", time.Since(start).Round(time.Millisecond))
	return response, status
}

func (s *Server) dispatch(ctx context.Context, req Request) (map[string]any, error) {
	if req["cmd"] == nil {
		return nil, errors.New("Request parameter 'cmd' is mandatory.")
	}
	if req["headers"] != nil {
		s.cfg.Logger.Warn("Request parameter 'headers' was removed in BlazeSolver v2.")
	}
	if req["userAgent"] != nil {
		s.cfg.Logger.Warn("Request parameter 'userAgent' was removed in BlazeSolver v2.")
	}
	timeout := int64(60000)
	if req["maxTimeout"] != nil {
		value, err := pythonInt(req["maxTimeout"])
		if err != nil {
			return nil, err
		}
		if value > 0 {
			timeout = value
		}
	}
	switch req["cmd"] {
	case "sessions.create":
		id := req["session"]
		if !truthy(id) {
			id = newSessionID()
		}
		entry, fresh, err := s.acquireSession(ctx, id, proxyValue(req["proxy"]), 0)
		if err != nil {
			return nil, err
		}
		id = entry.id
		entry.use.Unlock()
		message := "Session already exists."
		if fresh {
			message = "Session created successfully."
		}
		return map[string]any{"status": "ok", "message": message, "session": id}, nil
	case "sessions.list":
		return map[string]any{"status": "ok", "message": "", "sessions": s.sessions.ids()}, nil
	case "sessions.destroy":
		if err := checkSessionID(req["session"]); err != nil {
			return nil, err
		}
		destroyed, err := s.sessions.destroyContext(ctx, req["session"])
		if err != nil {
			return nil, err
		}
		if !destroyed {
			return nil, errors.New("The session doesn't exist.")
		}
		return map[string]any{"status": "ok", "message": "The session has been removed."}, nil
	case "request.get", "request.post":
		if req["cmd"] == "request.get" {
			if req["url"] == nil {
				return nil, errors.New("Request parameter 'url' is mandatory in 'request.get' command.")
			}
			if req["postData"] != nil {
				return nil, errors.New("Cannot use 'postBody' when sending a GET request.")
			}
		} else if req["postData"] == nil {
			return nil, errors.New("Request parameter 'postData' is mandatory in 'request.post' command.")
		}
		for _, name := range []string{"returnRawHtml", "download"} {
			if req[name] != nil {
				s.cfg.Logger.Warn("Request parameter '" + name + "' was removed in BlazeSolver v2.")
			}
		}
		return s.resolve(ctx, req, timeout)
	default:
		return nil, fmt.Errorf("Request parameter 'cmd' = '%s' is invalid.", textValue(req["cmd"]))
	}
}

func (s *Server) resolve(ctx context.Context, req Request, timeout int64) (map[string]any, error) {
	var browser *Browser
	var err error
	if truthy(req["session"]) {
		var ttl time.Duration
		if truthy(req["session_ttl_minutes"]) {
			minutes, parseErr := numericValue(req["session_ttl_minutes"])
			if parseErr != nil {
				return nil, solvingError(parseErr)
			}
			ttl = time.Duration(minutes * float64(time.Minute))
		}
		entry, _, acquireErr := s.acquireSession(ctx, req["session"], nil, ttl)
		err = acquireErr
		if err == nil {
			defer entry.use.Unlock()
			browser = entry.browser
		}
	} else {
		browser, err = s.launch(ctx, s.cfg, proxyValue(req["proxy"]), s.cfg.UserAgent)
		if err == nil {
			defer browser.Close()
		}
	}
	if err != nil {
		return nil, solvingError(err)
	}
	deadline, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	solution, message, err := s.solve(deadline, browser, req, s.cfg)
	if errors.Is(deadline.Err(), context.DeadlineExceeded) {
		seconds := strconv.FormatFloat(float64(timeout)/1000, 'f', -1, 64)
		if !strings.Contains(seconds, ".") {
			seconds += ".0"
		}
		return nil, fmt.Errorf("Error solving the challenge. Timeout after %s seconds.", seconds)
	}
	if err != nil {
		return nil, solvingError(err)
	}
	return map[string]any{"status": "ok", "message": message, "solution": solution}, nil
}

func solvingError(err error) error {
	return errors.New("Error solving the challenge. " + strings.ReplaceAll(err.Error(), "\n", `\n`))
}
func proxyValue(value any) map[string]any { object, _ := value.(map[string]any); return object }

func pythonInt(value any) (int64, error) {
	if text, ok := value.(string); ok {
		if !pythonIntegerText.MatchString(strings.TrimSpace(text)) {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pythonRepr(text))
		}
		n, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(text), "_", ""), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pythonRepr(text))
		}
		return n, nil
	}
	n, err := numericValue(value)
	if err != nil {
		return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pythonType(value))
	}
	if math.IsInf(n, 0) || math.IsNaN(n) || n >= math.MaxInt64 || n < math.MinInt64 {
		return 0, errors.New("integer timeout is out of range")
	}
	return int64(n), nil
}

var pythonIntegerText = regexp.MustCompile(`^[+-]?[0-9]+(?:_[0-9]+)*$`)

func pythonType(value any) string {
	switch value.(type) {
	case bool:
		return "bool"
	case string:
		return "str"
	case json.Number:
		if strings.ContainsAny(value.(json.Number).String(), ".eE") {
			return "float"
		}
		return "int"
	case float64:
		return "float"
	case int:
		return "int"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	default:
		return "NoneType"
	}
}
func pythonRepr(value string) string {
	quote := "'"
	if strings.Contains(value, "'") && !strings.Contains(value, `"`) {
		quote = `"`
	}
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, quote, `\`+quote)
	value = strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(value)
	return quote + value + quote
}

type savedSession struct {
	use     sync.Mutex
	id      any
	browser *Browser
	created time.Time
	ready   atomic.Bool
}
type sessionStore struct {
	mu      sync.Mutex
	entries map[string]*savedSession
	order   []string
	closed  bool
}

func sessionKey(id any) string {
	switch value := id.(type) {
	case bool:
		if value {
			return "number:1"
		}
		return "number:0"
	case json.Number:
		if !strings.ContainsAny(value.String(), ".eE") {
			if integer, ok := new(big.Int).SetString(value.String(), 10); ok {
				return "number:" + integer.String()
			}
		}
		if number, err := value.Float64(); err == nil {
			return numericSessionKey(number)
		}
	case float64, int:
		if number, err := numericValue(value); err == nil {
			return numericSessionKey(number)
		}
	}
	return fmt.Sprintf("%T:%v", id, id)
}

func numericSessionKey(number float64) string {
	if !math.IsInf(number, 0) && !math.IsNaN(number) && math.Trunc(number) == number {
		integer, _ := new(big.Float).SetFloat64(number).Int(nil)
		return "number:" + integer.String()
	}
	return "number:" + strconv.FormatFloat(number, 'g', -1, 64)
}

func checkSessionID(id any) error {
	switch id.(type) {
	case []any:
		return errors.New("unhashable type: 'list'")
	case map[string]any:
		return errors.New("unhashable type: 'dict'")
	}
	return nil
}

func lockSession(ctx context.Context, entry *savedSession) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if entry.use.TryLock() {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if entry.use.TryLock() {
				return nil
			}
		}
	}
}

func (s *Server) acquireSession(ctx context.Context, id any, proxy map[string]any, ttl time.Duration) (*savedSession, bool, error) {
	if err := checkSessionID(id); err != nil {
		return nil, false, err
	}
	key := sessionKey(id)
	for {
		s.sessions.mu.Lock()
		if s.sessions.closed {
			s.sessions.mu.Unlock()
			return nil, false, errors.New("server is shutting down")
		}
		entry := s.sessions.entries[key]
		if entry == nil {
			entry = &savedSession{id: id}
			s.sessions.entries[key] = entry
			s.sessions.order = append(s.sessions.order, key)
		}
		s.sessions.mu.Unlock()
		if err := lockSession(ctx, entry); err != nil {
			return nil, false, err
		}
		s.sessions.mu.Lock()
		current := s.sessions.entries[key] == entry
		s.sessions.mu.Unlock()
		if !current {
			entry.use.Unlock()
			continue
		}
		fresh := entry.browser == nil
		if !fresh && ttl != 0 && time.Since(entry.created) > ttl {
			_ = entry.browser.Close()
			entry.browser = nil
			fresh = true
			proxy = nil
		}
		if fresh {
			browser, err := s.launch(ctx, s.cfg, proxy, s.cfg.UserAgent)
			if err != nil {
				s.sessions.remove(key, entry)
				entry.use.Unlock()
				return nil, false, err
			}
			entry.browser, entry.created = browser, time.Now()
			entry.ready.Store(true)
		}
		return entry, fresh, nil
	}
}

func (s *sessionStore) remove(key string, entry *savedSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[key] != entry {
		return
	}
	delete(s.entries, key)
	for i, item := range s.order {
		if item == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}
func (s *sessionStore) ids() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := []any{}
	for _, key := range s.order {
		if entry := s.entries[key]; entry != nil && entry.ready.Load() {
			ids = append(ids, entry.id)
		}
	}
	return ids
}
func (s *sessionStore) destroy(id any) bool {
	destroyed, _ := s.destroyContext(context.Background(), id)
	return destroyed
}

func (s *sessionStore) destroyContext(ctx context.Context, id any) (bool, error) {
	key := sessionKey(id)
	s.mu.Lock()
	entry := s.entries[key]
	s.mu.Unlock()
	if entry == nil {
		return false, nil
	}
	if err := lockSession(ctx, entry); err != nil {
		return false, err
	}
	defer entry.use.Unlock()
	s.mu.Lock()
	current := s.entries[key] == entry
	s.mu.Unlock()
	if !current {
		return false, nil
	}
	s.remove(key, entry)
	if entry.browser != nil {
		_ = entry.browser.Close()
	}
	return true, nil
}
func (s *Server) Close() {
	s.sessions.mu.Lock()
	s.sessions.closed = true
	ids := make([]any, 0, len(s.sessions.entries))
	for _, entry := range s.sessions.entries {
		ids = append(ids, entry.id)
	}
	s.sessions.mu.Unlock()
	for _, id := range ids {
		s.sessions.destroy(id)
	}
}

func newSessionID() string {
	var random [8]byte
	_, _ = rand.Read(random[:])
	timestamp := uint64(time.Now().UnixNano()/100) + 0x01b21dd213814000
	sequence := binary.BigEndian.Uint16(random[:2])&0x3fff | 0x8000
	random[2] |= 1
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", uint32(timestamp), uint16(timestamp>>32), uint16(timestamp>>48)&0x0fff|0x1000, sequence, random[2:])
}
