// Command openviking-server is the Go reimplementation of the OpenViking
// context database HTTP service. It exposes 24 REST routers, MCP streamable
// HTTP, OAuth 2.1, WebDAV, Prometheus metrics, and embeds the Web Studio SPA.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/observability"
	"github.com/saker-ai/ctxhub/internal/server"
	"github.com/saker-ai/ctxhub/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "openviking-server:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "", "path to ov.conf (default search order: ./ov.conf, $OV_CONFIG_PATH, $HOME/.config/openviking/ov.conf, /etc/openviking/ov.conf)")
	addr := flag.String("addr", "", "override server.host:server.port")
	profile := flag.Bool("profile", false, "enable net/http/pprof on /debug/pprof")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return nil
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if *addr != "" {
		host, port, splitErr := net.SplitHostPort(*addr)
		if splitErr != nil {
			return fmt.Errorf("invalid --addr %q: %w", *addr, splitErr)
		}
		n, parseErr := strconv.Atoi(port)
		if parseErr != nil {
			return fmt.Errorf("invalid --addr port %q: %w", port, parseErr)
		}
		cfg.Server.Host = host
		cfg.Server.Port = n
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	tracerShutdown, err := observability.InitTracer(context.Background(), cfg.OTEL)
	if err != nil {
		return fmt.Errorf("init tracer: %w", err)
	}
	defer tracerShutdown(context.Background())

	app, cleanup, err := server.BuildApp(cfg)
	if err != nil {
		return fmt.Errorf("build app: %w", err)
	}
	defer cleanup()

	if *profile {
		app.EnablePprof()
	}

	srv := &http.Server{
		Addr:              cfg.Server.Addr(),
		Handler:           app.Router(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("openviking-server listening", "addr", srv.Addr, "version", version.Version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
	app.AsynqServer().Shutdown()
	slog.Info("server stopped")
	return nil
}
