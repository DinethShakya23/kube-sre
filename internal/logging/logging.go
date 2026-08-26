// Package logging sets up the process logger.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Level maps LOG_LEVEL to a slog level. Unknown names are INFO.
func Level(name string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARNING", "WARN":
		return slog.LevelWarn
	case "ERROR", "CRITICAL":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// New builds a logger writing to w in text or json format.
func New(w io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: Level(level)}
	if strings.EqualFold(format, "json") {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// Setup installs the process wide logger on stdout.
func Setup(level, format string) *slog.Logger {
	l := New(os.Stdout, level, format)
	slog.SetDefault(l)
	return l
}
