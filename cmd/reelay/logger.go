package main

import (
	"log/slog"
	"os"

	"github.com/TechXTT/reelay/internal/config"
)

func newLogger(cfg *config.Config, dev bool) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Logging.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	format := cfg.Logging.Format
	if dev {
		format = "text"
		level = slog.LevelDebug
	}

	opts := &slog.HandlerOptions{Level: level}
	if format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
