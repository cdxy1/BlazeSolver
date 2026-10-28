package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Host, Port, ChromePath, DriverPath, CacheDir, Language string
	Headless, DisableMedia, LogHTML, PrometheusEnabled     bool
	PrometheusPort                                         string
	BrowserWaitTimeout                                     time.Duration
	UserAgent, Version                                     string
	Proxy                                                  map[string]any
	Logger                                                 *slog.Logger
}

func loadConfig() (Config, error) {
	cfg := Config{
		Host: envOr("HOST", "0.0.0.0"), Port: envOr("PORT", "8191"),
		ChromePath: os.Getenv("CHROME_PATH"), DriverPath: os.Getenv("DRIVER_PATH"),
		CacheDir: envOr("DRIVER_CACHE_DIR", defaultDriverCache()),
		Language: os.Getenv("LANG"), Headless: envBool("HEADLESS", true),
		DisableMedia: envBool("DISABLE_MEDIA", false), LogHTML: envBool("LOG_HTML", false),
		PrometheusEnabled: envBool("PROMETHEUS_ENABLED", false), PrometheusPort: envOr("PROMETHEUS_PORT", "8192"),
		Version: "3.5.2",
	}
	seconds, err := strconv.Atoi(envOr("BROWSER_WAIT_TIMEOUT", "1"))
	if err != nil {
		return cfg, fmt.Errorf("BROWSER_WAIT_TIMEOUT: %w", err)
	}
	cfg.BrowserWaitTimeout = time.Duration(seconds) * time.Second
	var metadata struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(packageMetadata, &metadata) == nil && metadata.Version != "" {
		cfg.Version = metadata.Version
	}
	if value, exists := os.LookupEnv("PROXY_URL"); exists {
		cfg.Proxy = map[string]any{"url": value}
		username, hasUsername := os.LookupEnv("PROXY_USERNAME")
		password, hasPassword := os.LookupEnv("PROXY_PASSWORD")
		if hasUsername || hasPassword {
			cfg.Proxy["username"], cfg.Proxy["password"] = nil, nil
			if hasUsername {
				cfg.Proxy["username"] = username
			}
			if hasPassword {
				cfg.Proxy["password"] = password
			}
		}
	}
	var output io.Writer = os.Stderr
	if name := os.Getenv("LOG_FILE"); name != "" {
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			return cfg, err
		}
		file, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return cfg, err
		}
		output = io.MultiWriter(os.Stderr, file)
	}
	level := slog.LevelInfo
	switch strings.ToUpper(envOr("LOG_LEVEL", "info")) {
	case "DEBUG":
		level = slog.LevelDebug
	case "WARNING", "WARN":
		level = slog.LevelWarn
	case "ERROR", "CRITICAL":
		level = slog.LevelError
	}
	cfg.Logger = slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: level, ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
		if attr.Key == slog.TimeKey {
			return slog.String("time", attr.Value.Time().Format("15:04:05.000"))
		}
		return attr
	}}))
	return cfg, nil
}

func defaultDriverCache() string {
	// Browser profiles may live on a noexec tmpfs; the driver executable must not.
	if _, err := os.Stat("/app/chromedriver"); err == nil {
		if directory, err := os.Stat("/config"); err == nil && directory.IsDir() {
			return "/config/drivers"
		}
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "blazesolver", "drivers")
}

func envOr(name, fallback string) string {
	if value, exists := os.LookupEnv(name); exists {
		return value
	}
	return fallback
}
func envBool(name string, fallback bool) bool {
	value, exists := os.LookupEnv(name)
	if !exists {
		return fallback
	}
	return strings.EqualFold(value, "true")
}

type Request map[string]any

func textValue(value any) string {
	switch value := value.(type) {
	case nil:
		return "None"
	case string:
		return value
	case bool:
		if value {
			return "True"
		}
		return "False"
	case []any:
		parts := make([]string, len(value))
		for i, item := range value {
			if text, ok := item.(string); ok {
				parts[i] = pythonRepr(text)
			} else {
				parts[i] = textValue(item)
			}
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		parts := make([]string, 0, len(value))
		for key, item := range value {
			rendered := textValue(item)
			if text, ok := item.(string); ok {
				rendered = pythonRepr(text)
			}
			parts = append(parts, pythonRepr(key)+": "+rendered)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(value)
	}
}

func truthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	case json.Number:
		number, _ := value.Float64()
		return number != 0
	case float64:
		return value != 0
	case int:
		return value != 0
	case []any:
		return len(value) > 0
	case map[string]any:
		return len(value) > 0
	default:
		return true
	}
}

func numericValue(value any) (float64, error) {
	switch value := value.(type) {
	case bool:
		if value {
			return 1, nil
		}
		return 0, nil
	case json.Number:
		return value.Float64()
	case float64:
		return value, nil
	case int:
		return float64(value), nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(value), 64)
	default:
		return 0, fmt.Errorf("a number is required, got %s", textValue(value))
	}
}
