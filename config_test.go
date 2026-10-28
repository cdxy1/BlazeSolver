package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConfigurationEnvironment(t *testing.T) {
	for name, value := range map[string]string{"HOST": "127.0.0.1", "PORT": "9999", "HEADLESS": "FALSE", "DISABLE_MEDIA": "TRUE", "LOG_HTML": "true", "BROWSER_WAIT_TIMEOUT": "3", "PROMETHEUS_ENABLED": "true", "PROMETHEUS_PORT": "9998", "PROXY_URL": "http://proxy:8888", "PROXY_USERNAME": "alice", "PROXY_PASSWORD": "secret", "LANG": "ru_RU.UTF-8", "LOG_LEVEL": "DEBUG", "LOG_FILE": filepath.Join(t.TempDir(), "nested", "service.log")} {
		t.Setenv(name, value)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Headless || !cfg.DisableMedia || !cfg.LogHTML || !cfg.PrometheusEnabled || cfg.BrowserWaitTimeout != 3*time.Second || cfg.Port != "9999" || cfg.PrometheusPort != "9998" {
		t.Fatalf("%+v", cfg)
	}
	if !reflect.DeepEqual(cfg.Proxy, map[string]any{"url": "http://proxy:8888", "username": "alice", "password": "secret"}) {
		t.Fatal(cfg.Proxy)
	}
	cfg.Logger.Debug("configuration test")
	data, err := os.ReadFile(os.Getenv("LOG_FILE"))
	if err != nil || len(data) == 0 {
		t.Fatal("log file not written", err)
	}
}

func TestConfigurationInvalidWait(t *testing.T) {
	t.Setenv("BROWSER_WAIT_TIMEOUT", "bad")
	if _, err := loadConfig(); err == nil {
		t.Fatal("invalid browser wait accepted")
	}
}

func TestDriverCacheAvoidsTemporaryProfiles(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	cache := defaultDriverCache()
	if !strings.HasSuffix(cache, "drivers") {
		t.Fatal("driver cache must be separate from browser profiles", cache)
	}
}

func TestPythonTruthiness(t *testing.T) {
	for _, value := range []any{nil, false, "", 0, float64(0), json.Number("0"), []any{}, map[string]any{}} {
		if truthy(value) {
			t.Errorf("unexpected true: %#v", value)
		}
	}
	for _, value := range []any{true, "false", -1, json.Number("1"), []any{nil}, map[string]any{"a": nil}} {
		if !truthy(value) {
			t.Errorf("unexpected false: %#v", value)
		}
	}
}
