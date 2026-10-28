package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDriverPatchMatchesVendoredUndetected(t *testing.T) {
	block := []byte("{window.cdc_abcdefghijklmnopqrstuvwxyz_Array = window.Array;}")
	original := append([]byte("before\x00"), block...)
	original = append(original, []byte("\x00after")...)
	patched, err := patchDriver(original)
	if err != nil {
		t.Fatal(err)
	}
	replacement := []byte(`{console.log("undetected chromedriver 1337!")}`)
	expected := bytes.Clone(original)
	position := bytes.Index(expected, block)
	for index := range block {
		expected[position+index] = ' '
	}
	copy(expected[position:], replacement)
	if !bytes.Equal(patched, expected) {
		t.Fatalf("patch differs from Python replacement: %q", patched)
	}
	if len(patched) != len(original) {
		t.Fatal("binary offsets changed")
	}
	again, err := patchDriver(patched)
	if err != nil || !bytes.Equal(patched, again) {
		t.Fatal("patch is not idempotent", err)
	}
	if !bytes.Contains(original, block) {
		t.Fatal("patch mutated source bytes")
	}
}

func TestDriverPatchRejectsUnknownBinaries(t *testing.T) {
	if _, err := patchDriver([]byte("not a known ChromeDriver executable")); err == nil {
		t.Fatal("unpatched driver was accepted")
	}
}

func TestPreparedDriverLeavesSourceUntouchedAndReusesCache(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "original")
	original := []byte("{window.cdc_abcdefghijklmnopqrstuvwxyz_Array = window.Array;}")
	if err := os.WriteFile(source, original, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{DriverPath: source, CacheDir: filepath.Join(directory, "cache")}
	first, err := prepareDriver(context.Background(), cfg, "unused-chrome")
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepareDriver(context.Background(), cfg, "unused-chrome")
	if err != nil || first != second {
		t.Fatal("cached driver not reused", err)
	}
	data, _ := os.ReadFile(first)
	if !bytes.Contains(data, []byte("undetected chromedriver")) {
		t.Fatal("cached driver not patched")
	}
	data, _ = os.ReadFile(source)
	if !bytes.Equal(data, original) {
		t.Fatal("original executable changed")
	}
}

func TestBrowserProtocolAndEmptyScriptArguments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/test-session/execute/sync" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if _, ok := payload["args"].([]any); !ok {
			t.Errorf("args must be an array: %#v", payload)
		}
		fmt.Fprint(w, `{"value":"Chrome/test"}`)
	}))
	defer server.Close()
	browser := newBrowserClient(server.URL, "test-session")
	defer browser.Client.CloseIdleConnections()
	value, err := browser.Execute(context.Background(), "return navigator.userAgent")
	if err != nil || value != "Chrome/test" {
		t.Fatal(value, err)
	}
}

func TestBrowserProtocolPreservesDriverErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"value":{"error":"no such element","message":"Unable to locate element"}}`)
	}))
	defer server.Close()
	browser := newBrowserClient(server.URL, "test")
	defer browser.Client.CloseIdleConnections()
	_, err := browser.Call(context.Background(), http.MethodGet, "/element/test", nil)
	if err == nil || err.Error() != "Message: Unable to locate element\n" {
		t.Fatal(err)
	}
}

func TestChromeArgumentsKeepUndetectedLaunch(t *testing.T) {
	args := chromeArguments(Config{Headless: true, Language: "en-US"}, "Chrome/test", "1234", "/tmp/profile")
	joined := strings.Join(args, " ")
	for _, expected := range []string{"--headless=new", "--user-agent=Chrome/test", "--remote-debugging-port=1234", "--user-data-dir=/tmp/profile", "--lang=en-US", "--accept-lang=en-US"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("missing %s", expected)
		}
	}
	for _, arg := range args {
		if strings.Contains(arg, "enable-automation") || strings.Contains(arg, "remote-debugging-pipe") {
			t.Errorf("unexpected automation flag %s", arg)
		}
	}
	features := 0
	for _, arg := range args {
		if strings.HasPrefix(arg, "--disable-features=") {
			features++
		}
	}
	if features != 1 {
		t.Fatal("Chrome feature flags overwrite each other")
	}
}

func TestProxyExtensionEncodesUntrustedCredentials(t *testing.T) {
	username := `someone";globalThis.injected=true;//`
	directory, err := createProxyExtension(map[string]any{"url": "http://localhost:8080", "username": username, "password": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	script, err := os.ReadFile(filepath.Join(directory, "background.js"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(username)
	if !strings.Contains(string(script), "username:"+string(encoded)) {
		t.Fatal("username interpolated without JSON encoding")
	}
	var manifest map[string]any
	data, _ := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["manifest_version"] != float64(3) {
		t.Fatal("proxy extension changed manifest version")
	}
}
