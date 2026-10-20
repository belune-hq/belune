package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/belune-hq/belune/internal/app"
	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/pkg/logger"
	"github.com/belune-hq/belune/internal/pkg/tracing"
	"github.com/belune-hq/belune/internal/version"
)

func main() {
	// Answered before anything else is loaded, so it works on a host with no
	// config, no database, and no network — which is exactly when someone is
	// asking "what is actually running here?".
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(version.Version)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	var logLevel slog.Level
	if err := logLevel.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		logLevel = slog.LevelInfo
	}
	// Console by default — the JSON handler is unreadable in a terminal. Both
	// stay wrapped in RedactHandler, which strips secrets (deploy hook tokens
	// and friends) from messages and attributes: dropping that wrapper would
	// print them.
	var base slog.Handler
	if strings.EqualFold(cfg.LogFormat, "json") {
		base = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	} else {
		opts := &slog.HandlerOptions{Level: logLevel}
		switch strings.ToLower(cfg.LogColor) {
		case "always":
			base = logger.NewConsoleHandlerWithColor(os.Stdout, opts, true)
		case "never":
			base = logger.NewConsoleHandlerWithColor(os.Stdout, opts, false)
		default: // auto — colour only when stdout is a terminal
			base = logger.NewConsoleHandler(os.Stdout, opts)
		}
	}
	slog.SetDefault(slog.New(logger.NewRedactHandler(base)))

	// Configuration findings: accepted but inadvisable. Logged once the logger
	// exists so they honour the operator's format and level, and never fatal —
	// the dashboard shows the same list via /api/stats.
	for _, w := range cfg.Validate() {
		slog.Warn("configuration warning", "code", w.Code, "detail", w.Message, "remedy", w.Remedy)
	}

	traceShutdown, err := tracing.Init(context.Background(), tracing.Config{
		Endpoint:       cfg.OTLPEndpoint,
		Insecure:       cfg.OTLPInsecure,
		ServiceName:    "belune-api",
		ServiceVersion: version.Version,
	})
	if err != nil {
		slog.Error("failed to init tracing", "error", err)
		os.Exit(1)
	}

	a, err := app.New(cfg)
	if err != nil {
		slog.Error("failed to initialise application", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := a.Run(ctx); err != nil {
		slog.Error("application error", "error", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
	if err := traceShutdown(shutdownCtx); err != nil {
		slog.Error("tracing shutdown error", "error", err)
	}
}
