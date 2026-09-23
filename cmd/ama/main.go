// Command ama is the AMA daemon: a single process that polls the optical
// drive, identifies and rips whatever disc appears, and serves the web UI
// that confirms identification and reports progress.
//
// main itself only parses flags, loads and validates config, builds a
// logger, and hands off to daemon.run — see docs/CLAUDE.md's build order
// and docs/ARCHITECTURE.md's "Rip Flow" for what the daemon actually does.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/sarumont/ama/config"
	"github.com/sarumont/ama/internal/manifest"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ama:", err)
		os.Exit(1)
	}
}

// run is the whole program as a testable function: parse, construct, run.
// It never calls os.Exit itself — main is the only place that does.
func run(args []string, stdout, stderr *os.File) error {
	fset := newFlagSet(stderr)
	configPath, showVersion, err := parseFlags(fset, args)
	if err != nil {
		if errors.Is(err, errFlagHelp) {
			return nil
		}
		return err
	}
	if showVersion {
		_, err := fmt.Fprintln(stdout, manifest.Version)
		return err
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	logger := newLogger(stdout)
	slog.SetDefault(logger)
	resolvedConfigPath, _ := config.ResolvePath(configPath)
	logger.Info("ama starting", "version", manifest.Version, "config", resolvedConfigPath)

	d, err := newDaemon(cfg, logger)
	if err != nil {
		return fmt.Errorf("constructing daemon: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := d.checkTools(ctx); err != nil {
		return fmt.Errorf("startup checks: %w", err)
	}

	return d.run(ctx)
}

// newLogger builds the daemon's structured logger. The level comes from
// AMA_LOG_LEVEL (debug/info/warn/error, case-insensitive) rather than a
// config.Config field: it is an operational knob for container log verbosity,
// not a YAML-documented setting a rip's behavior depends on. Unset or
// unrecognized defaults to info.
func newLogger(w *os.File) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AMA_LOG_LEVEL"))) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}
