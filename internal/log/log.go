// Package log configures structured logging for toad.
package log

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// logFile holds the current log file handle so it can be closed on subsequent
// calls to Setup (the daemon may call Setup twice: once before config validation
// and again after the init wizard).
var logFile *os.File

// Setup initializes the global slog logger with the given level and optional file output.
// It closes any previously opened log file to avoid handle leaks.
func Setup(level, file string) {
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}

	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	var w io.Writer = os.Stderr
	if file != "" {
		dir := filepath.Dir(file)
		if err := os.MkdirAll(dir, 0o755); err == nil {
			if f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				logFile = f
				w = io.MultiWriter(os.Stderr, f)
			}
		}
	}

	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(handler))
}
