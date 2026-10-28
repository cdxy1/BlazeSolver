package main

import (
	"context"
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

//go:embed package.json
var packageMetadata []byte

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "BlazeSolver:", err)
		os.Exit(1)
	}
}

func run() error {
	defer StopBrowserDisplay()
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	printBanner(cfg.Version)
	cfg.Logger.Info("Checking undetected browser")
	browser, err := LaunchBrowser(ctx, cfg, nil, "")
	if err != nil {
		return err
	}
	agent, err := browser.Execute(ctx, "return navigator.userAgent")
	closeErr := browser.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		cfg.Logger.Debug("Startup browser cleanup", "error", closeErr)
	}
	cfg.UserAgent = headlessWord.ReplaceAllString(textValue(agent), "")
	app := NewServer(cfg)
	defer app.Close()
	server := &http.Server{Addr: net.JoinHostPort(cfg.Host, cfg.Port), Handler: app, ReadHeaderTimeout: 10 * time.Second}
	errors := make(chan error, 2)
	go func() { errors <- server.ListenAndServe() }()
	var exporter *http.Server
	if cfg.PrometheusEnabled {
		exporter = &http.Server{Addr: net.JoinHostPort("0.0.0.0", cfg.PrometheusPort), Handler: app.metrics, ReadHeaderTimeout: 10 * time.Second}
		go func() { errors <- exporter.ListenAndServe() }()
	}
	cfg.Logger.Info("Ready", "address", server.Addr, "version", cfg.Version, "user_agent", cfg.UserAgent)
	select {
	case <-ctx.Done():
	case err = <-errors:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	if exporter != nil {
		_ = exporter.Shutdown(shutdown)
	}
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func printBanner(version string) {
	color, reset := "", ""
	if info, err := os.Stderr.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == "" {
		color, reset = "\033[38;5;208m", "\033[0m"
	}
	const banner = `
        (
       ( )
      (   )    BlazeSolver
     (  (  )   Undetected Chromium · Go
      \   /    v%s
       \_/
`
	fmt.Fprintf(os.Stderr, "%s"+banner+"%s\n", color, version, reset)
}
