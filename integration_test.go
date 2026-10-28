//go:build integration

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRealUndetectedBrowser(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/favicon.ico":
			w.WriteHeader(204)
		case "/blocked":
			fmt.Fprint(w, `<html><title>Access denied</title><body>blocked</body></html>`)
		case "/challenge":
			fmt.Fprint(w, `<html><title>Just a moment...</title><body><div id="cf-challenge-running"></div><script>setTimeout(()=>{document.title='Done';document.querySelector('div').remove()},700)</script></body></html>`)
		case "/turnstile":
			fmt.Fprint(w, `<html><title>Normal</title><body><button onclick="document.querySelector('input').value='local-token'">Verify</button><input type="hidden" name="cf-turnstile-response" value=""></body></html>`)
		case "/post":
			data, _ := io.ReadAll(r.Body)
			fmt.Fprintf(w, "<html><title>POST</title><body>%s</body></html>", data)
		case "/state":
			fmt.Fprintf(w, "<html><title>State</title><body>%s</body></html>", r.Header.Get("Cookie"))
		case "/slow":
			time.Sleep(500 * time.Millisecond)
			fmt.Fprint(w, `<html><title>Slow</title></html>`)
		default:
			fmt.Fprint(w, `<html><title>Example</title><body>hello</body></html>`)
		}
	}))
	defer fixture.Close()
	cfg := Config{Headless: envBool("HEADLESS", true), ChromePath: envOr("CHROME_PATH", ""), DriverPath: envOr("DRIVER_PATH", ""), CacheDir: t.TempDir(), BrowserWaitTimeout: time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "1.0.0"}
	server := NewServer(cfg)
	defer server.Close()
	call := func(req Request) (int, map[string]any) {
		t.Helper()
		data, _ := json.Marshal(req)
		status, body, _ := apiRequest(t, server, "POST", "/v1", string(data), "application/json")
		return status, body
	}
	requireOK := func(req Request) map[string]any {
		t.Helper()
		status, body := call(req)
		if status != 200 {
			t.Fatalf("%d %#v", status, body)
		}
		return body
	}
	requireOK(Request{"cmd": "sessions.create", "session": "integration"})
	get := func(path string, extra Request) map[string]any {
		t.Helper()
		req := Request{"cmd": "request.get", "url": fixture.URL + path, "session": "integration"}
		for key, value := range extra {
			req[key] = value
		}
		return requireOK(req)
	}
	t.Run("normal", func(t *testing.T) {
		result := get("/", nil)
		solution := result["solution"].(map[string]any)
		if !strings.Contains(solution["response"].(string), "hello") || solution["status"] != float64(200) {
			t.Fatal(result)
		}
	})
	t.Run("undetected-fingerprint", func(t *testing.T) {
		entry, _, err := server.acquireSession(context.Background(), "integration", nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer entry.use.Unlock()
		value, err := entry.browser.Execute(context.Background(), `return {webdriver:navigator.webdriver,cdc:Object.keys(window).filter(k=>/^cdc_/.test(k))}`)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint := value.(map[string]any)
		if fingerprint["webdriver"] != false || len(fingerprint["cdc"].([]any)) != 0 {
			t.Fatalf("automation fingerprint changed: %#v", fingerprint)
		}
	})
	t.Run("cookie-and-cookie-only", func(t *testing.T) {
		result := get("/state", Request{"cookies": []any{map[string]any{"name": "integration", "value": "saved"}}, "returnOnlyCookies": true, "returnScreenshot": true})
		solution := result["solution"].(map[string]any)
		if _, exists := solution["response"]; exists {
			t.Fatal("cookie-only returned HTML")
		}
		if _, exists := solution["headers"]; exists {
			t.Fatal("cookie-only returned headers")
		}
		image, err := base64.StdEncoding.DecodeString(solution["screenshot"].(string))
		if err != nil || !strings.HasPrefix(string(image), "\x89PNG") {
			t.Fatal("invalid screenshot", err)
		}
		state := get("/state", nil)["solution"].(map[string]any)["response"].(string)
		if !strings.Contains(state, "integration=saved") {
			t.Fatal("session lost cookies", state)
		}
	})
	t.Run("post", func(t *testing.T) {
		result := requireOK(Request{"cmd": "request.post", "session": "integration", "url": fixture.URL + "/post", "postData": "a=one&b=two"})
		if !strings.Contains(result["solution"].(map[string]any)["response"].(string), "a=one&amp;b=two") {
			t.Fatal(result)
		}
	})
	t.Run("challenge-transition", func(t *testing.T) {
		if result := get("/challenge", nil); result["message"] != "Challenge solved!" {
			t.Fatal(result)
		}
	})
	t.Run("blocked", func(t *testing.T) {
		status, body := call(Request{"cmd": "request.get", "session": "integration", "url": fixture.URL + "/blocked"})
		if status != 500 || !strings.Contains(body["message"].(string), "Cloudflare has blocked this request") {
			t.Fatal(status, body)
		}
	})
	t.Run("turnstile-actions", func(t *testing.T) {
		result := get("/turnstile", Request{"tabs_till_verify": 1})
		if result["solution"].(map[string]any)["turnstile_token"] != "local-token" {
			t.Fatal(result)
		}
	})
	t.Run("ephemeral-isolation", func(t *testing.T) {
		result := requireOK(Request{"cmd": "request.get", "url": fixture.URL + "/state"})
		if len(result["solution"].(map[string]any)["cookies"].([]any)) != 0 {
			t.Fatal("session cookies leaked", result)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		status, body := call(Request{"cmd": "request.get", "url": fixture.URL + "/slow", "maxTimeout": 10})
		if status != 500 || body["message"] != "Error: Error solving the challenge. Timeout after 0.01 seconds." {
			t.Fatal(status, body)
		}
	})
	t.Run("ttl-replacement", func(t *testing.T) {
		result := get("/state", Request{"session_ttl_minutes": -1})
		if len(result["solution"].(map[string]any)["cookies"].([]any)) != 0 {
			t.Fatal("TTL browser retained cookies", result)
		}
	})
}
