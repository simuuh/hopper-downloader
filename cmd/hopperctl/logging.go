package main

import (
	"log/slog"
	"os"
	"strings"
)

// newLogger builds a text-handler slog.Logger writing to stderr, per
// docs/ARCHITECTURE.md's structured-logging convention (no package-level default
// logger — constructed explicitly and passed in). Text, not JSON: unlike
// hopperd, hopperctl's log output is read directly by an operator's
// terminal or `journalctl`/Console.app, not shipped to a log aggregator.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
