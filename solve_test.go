package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

type browserCommand struct {
	method, path string
	payload      any
}
type fakePageBrowser struct {
	calls   []browserCommand
	respond func(context.Context, string, string, any) (any, error)
}

func (b *fakePageBrowser) Call(ctx context.Context, method, path string, payload any) (any, error) {
	b.calls = append(b.calls, browserCommand{method, path, payload})
	if b.respond != nil {
		value, err := b.respond(ctx, method, path, payload)
		if value != nil || err != nil {
			return value, err
		}
	}
	switch path {
	case "/title":
		return "Example", nil
	case "/url":
		return "https://example.com/final", nil
	case "/source":
		return "<html><body>Example</body></html>", nil
	case "/cookie":
		if method == "GET" {
			return []any{map[string]any{"name": "session", "value": "abc"}}, nil
		}
	case "/screenshot":
		return "c2NyZWVuc2hvdA==", nil
	case "/elements":
		return []any{}, nil
	case "/element":
		return map[string]any{elementKey: "html-id"}, nil
	case "/execute/sync":
		return "Chrome/Test", nil
	}
	return nil, nil
}
func (b *fakePageBrowser) Execute(ctx context.Context, script string, args ...any) (any, error) {
	return b.Call(ctx, "POST", "/execute/sync", map[string]any{"script": script, "args": args})
}
func (b *fakePageBrowser) Navigate(ctx context.Context, target string) error {
	_, err := b.Call(ctx, "POST", "/url", map[string]any{"url": target})
	return err
}
func (b *fakePageBrowser) CDP(ctx context.Context, command string, params any) (any, error) {
	return b.Call(ctx, "POST", "/goog/cdp/execute", map[string]any{"cmd": command, "params": params})
}
func (b *fakePageBrowser) count(method, path string) int {
	count := 0
	for _, call := range b.calls {
		if call.method == method && call.path == path {
			count++
		}
	}
	return count
}
func solverConfig() Config {
	return Config{UserAgent: "Chrome/Test", BrowserWaitTimeout: time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}
func getRequest() Request { return Request{"cmd": "request.get", "url": "https://example.com/"} }

func TestSolveNormalPageContract(t *testing.T) {
	browser := &fakePageBrowser{}
	solution, message, err := solvePage(context.Background(), browser, getRequest(), solverConfig())
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{"url": "https://example.com/final", "status": 200, "userAgent": "Chrome/Test", "turnstile_token": nil, "headers": map[string]any{}, "response": "<html><body>Example</body></html>", "cookies": []any{map[string]any{"name": "session", "value": "abc"}}}
	if !reflect.DeepEqual(solution, expected) {
		t.Fatalf("solution mismatch: %#v", solution)
	}
	if message != "Challenge not detected!" {
		t.Fatalf("message: %q", message)
	}
	if browser.count("POST", "/elements") != 2 {
		t.Fatalf("expected 2 initial queries, got %#v", browser.calls)
	}
	if browser.count("POST", "/execute/sync") != 0 {
		t.Fatal("cached user agent should not execute JS")
	}
}

func TestSolveCookieOnlyScreenshotAndCookieReload(t *testing.T) {
	browser := &fakePageBrowser{}
	req := getRequest()
	req["returnOnlyCookies"] = true
	req["returnScreenshot"] = true
	req["waitInSeconds"] = float64(60)
	req["cookies"] = []any{map[string]any{"name": "session/key", "value": "provided"}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	solution, _, err := solvePage(ctx, browser, req, solverConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := solution["headers"]; ok {
		t.Fatal("headers must be omitted")
	}
	if _, ok := solution["response"]; ok {
		t.Fatal("response must be omitted")
	}
	if solution["screenshot"] != "c2NyZWVuc2hvdA==" {
		t.Fatal(solution)
	}
	if browser.count("GET", "/source") != 0 {
		t.Fatal("cookie-only should not retrieve source")
	}
	if browser.count("POST", "/url") != 2 {
		t.Fatal("cookies must trigger navigation after insertion")
	}
	if browser.count("DELETE", "/cookie/session%2Fkey") != 1 || browser.count("POST", "/cookie") != 1 {
		t.Fatal(browser.calls)
	}
	if last := browser.calls[len(browser.calls)-1]; last.method != "GET" || last.path != "/cookie" {
		t.Fatal("cookies must be read last")
	}
}

func TestSolveDeniedPages(t *testing.T) {
	for _, title := range []string{"Access denied", "Access denied: site", "Attention Required! | Cloudflare"} {
		t.Run(title, func(t *testing.T) {
			browser := &fakePageBrowser{respond: func(ctx context.Context, method, path string, payload any) (any, error) {
				if path == "/title" {
					return title, nil
				}
				return nil, nil
			}}
			_, _, err := solvePage(context.Background(), browser, getRequest(), solverConfig())
			if err == nil || err.Error() != accessDeniedError().Error() {
				t.Fatalf("wrong error: %v", err)
			}
			if browser.count("GET", "/source") != 0 {
				t.Fatal("blocked page should not be returned")
			}
		})
	}
	browser := &fakePageBrowser{respond: func(ctx context.Context, method, path string, payload any) (any, error) {
		if path == "/elements" && payload.(map[string]any)["value"] == strings.Join(deniedSelectors, ", ") {
			return []any{map[string]any{elementKey: "denied"}}, nil
		}
		return nil, nil
	}}
	if _, _, err := solvePage(context.Background(), browser, getRequest(), solverConfig()); err == nil || err.Error() != accessDeniedError().Error() {
		t.Fatalf("wrong selector error: %v", err)
	}
}

func TestSolveChallengeWaitsForAllOriginalSelectors(t *testing.T) {
	browser := &fakePageBrowser{respond: func(ctx context.Context, method, path string, payload any) (any, error) {
		if path == "/elements" && payload.(map[string]any)["value"] == strings.Join(challengeSelectors, ", ") {
			return []any{map[string]any{elementKey: "challenge"}}, nil
		}
		if strings.HasSuffix(path, "/enabled") {
			return nil, errors.New("stale element reference")
		}
		return nil, nil
	}}
	_, message, err := solvePage(context.Background(), browser, getRequest(), solverConfig())
	if err != nil {
		t.Fatal(err)
	}
	if message != "Challenge solved!" {
		t.Fatal(message)
	}
	var selectors []string
	for _, call := range browser.calls {
		if call.path == "/elements" {
			selectors = append(selectors, call.payload.(map[string]any)["value"].(string))
		}
	}
	expected := append([]string{strings.Join(deniedSelectors, ", "), strings.Join(challengeSelectors, ", ")}, challengeSelectors...)
	if !reflect.DeepEqual(selectors, expected) {
		t.Fatalf("wait selectors changed: %#v", selectors)
	}
	if browser.count("GET", "/title") != 1+len(challengeTitles) {
		t.Fatal("individual title waits changed")
	}
	if browser.count("POST", "/actions") != 0 {
		t.Fatal("already disappeared challenge must not click")
	}
}

func TestSolveWaitHonorsRequestCancellation(t *testing.T) {
	browser := &fakePageBrowser{}
	req := getRequest()
	req["waitInSeconds"] = float64(60)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, _, err := solvePage(ctx, browser, req, solverConfig())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", err)
	}
	if browser.count("GET", "/source") != 0 || browser.count("GET", "/cookie") != 0 {
		t.Fatal("must stop before collecting results")
	}
}

func TestMediaFailureIsBestEffortAndRequestOverridesEnvironment(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		browser := &fakePageBrowser{respond: func(ctx context.Context, method, path string, payload any) (any, error) {
			if path == "/goog/cdp/execute" {
				return nil, errors.New("unsupported")
			}
			return nil, nil
		}}
		req := getRequest()
		req["disableMedia"] = enabled
		cfg := solverConfig()
		cfg.DisableMedia = true
		if _, _, err := solvePage(context.Background(), browser, req, cfg); err != nil {
			t.Fatal(err)
		}
		expected := 0
		if enabled {
			expected = 1
		}
		if browser.count("POST", "/goog/cdp/execute") != expected {
			t.Fatal(browser.calls)
		}
	}
}

func TestPostFormPreservesLegacyEncoding(t *testing.T) {
	req := Request{"url": "https://example.com/post", "postData": "?name=hello+world&slash=a%2Fb&quote=%22&submit=ignored&flag&x=%FF&bad=%ZZ"}
	form := postFormURL(req)
	for _, fragment := range []string{`action="https://example.com/post" method="POST"`, `name="name" value="hello%2Bworld"`, `name="slash" value="a/b"`, `name="quote" value="%26quot%3B"`, `name="flag" value=""`, `name="x" value="%EF%BF%BD"`, `name="bad" value="%25ZZ"`, `document.getElementById('hackForm').submit();`} {
		if !strings.Contains(form, fragment) {
			t.Fatalf("missing %q in %s", fragment, form)
		}
	}
	if strings.Contains(form, `name="submit"`) {
		t.Fatal("submit name must be skipped")
	}
	req["postData"] = ""
	if !strings.Contains(postFormURL(req), `name="" value=""`) {
		t.Fatal("empty postData still creates legacy empty input")
	}
}

func TestPostCookieReloadSubmitsFormTwice(t *testing.T) {
	browser := &fakePageBrowser{}
	req := Request{"cmd": "request.post", "url": "https://example.com/post", "postData": "a=b", "cookies": []any{map[string]any{"name": "a", "value": "b"}}}
	if _, _, err := solvePage(context.Background(), browser, req, solverConfig()); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, call := range browser.calls {
		if call.path == "/url" && call.method == "POST" {
			count++
			if !strings.HasPrefix(call.payload.(map[string]any)["url"].(string), "data:text/html;charset=utf-8,") {
				t.Fatal("POST must navigate through form")
			}
		}
	}
	if count != 2 {
		t.Fatal(count)
	}
}

func TestSolverPropagatesBrowserFailure(t *testing.T) {
	failure := errors.New("browser disconnected")
	browser := &fakePageBrowser{respond: func(ctx context.Context, method, path string, payload any) (any, error) {
		if path == "/source" {
			return nil, failure
		}
		return nil, nil
	}}
	if _, _, err := solvePage(context.Background(), browser, getRequest(), solverConfig()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

func TestLogHTMLRequiresDebugAndUserAgentFallback(t *testing.T) {
	browser := &fakePageBrowser{}
	req := getRequest()
	req["returnOnlyCookies"] = true
	cfg := solverConfig()
	cfg.LogHTML = true
	cfg.UserAgent = ""
	result, _, err := solvePage(context.Background(), browser, req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if browser.count("GET", "/source") != 0 {
		t.Fatal("INFO must not fetch log HTML")
	}
	if result["userAgent"] != "Chrome/Test" {
		t.Fatal(result)
	}
}

func TestWaitForPageKeepsPollingAndStopsOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := waitForPage(ctx, time.Second, func() (bool, error) { called = true; return true, nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("cancellation ignored: %v", err)
	}
	err = waitForPage(context.Background(), 0, func() (bool, error) { return false, nil })
	if !errors.Is(err, errChallengeWait) {
		t.Fatal(err)
	}
}

func TestPostUnquoteMatchesPythonMalformedUTF8(t *testing.T) {
	for encoded, expected := range map[string]string{
		"%E2%82": "�", "%E2%28%A1": "�(�", "%E0%80%80": "���", "%ED%A0%80": "���",
		"%F0%90%80": "�", "%F4%90%80%80": "����", "%C2": "�", "%FF%FF": "��", "%ZZ": "%ZZ",
	} {
		if actual := unquotePost(encoded); actual != expected {
			t.Errorf("%s: got %q want %q", encoded, actual, expected)
		}
	}
}
