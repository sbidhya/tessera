package logger

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New creates a structured *slog.Logger according to level and format.
//
// level  — debug | info | warn | error  (case-insensitive, defaults to info)
// format — json | text                   (case-insensitive, defaults to json)
// out    — destination, defaults to os.Stdout when nil
//
// Design notes:
//   - We use the standard library's log/slog so the project has zero extra
//     logging dependencies (project rule: "standard library first").
//   - JSON is the default because it is the only format that downstream log
//     aggregators (CloudWatch, Loki, etc.) can reliably parse without regex.
//     "text" stays available for local dev where human readability wins.
//   - slog is allocation-aware and supports With() / WithGroup() for adding
//     request-scoped fields without string interpolation — prefer
//     logger.Info("room created", "room_id", id) over fmt.Sprintf.
func New(level, format string, out io.Writer) *slog.Logger {
	if out == nil {
		out = os.Stdout
	}

	lvl := parseLevel(level)

	opts := &slog.HandlerOptions{
		Level: lvl,
		// AddSource adds file:line to every record at debug level only would be
		// noisy; leave it off by default. Callers can create a child logger
		// with AddSource:true when debugging.
		AddSource: false,
	}

	var handler slog.Handler
	switch strings.ToLower(format) {
	case "text":
		handler = slog.NewTextHandler(out, opts)
	default: // "json" and any unknown value falls back to JSON — never silently drop logs.
		handler = slog.NewJSONHandler(out, opts)
	}

	return slog.New(handler)
}

// parseLevel maps a string to slog.Level; unknown values become Info.
func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "info", "":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// NewDefault is a convenience that reads level/format from the provided
// strings and writes to os.Stdout. It exists so main.go stays one line.
func NewDefault(level, format string) *slog.Logger {
	return New(level, format, os.Stdout)
}
