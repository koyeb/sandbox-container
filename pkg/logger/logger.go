package logger

import "log/slog"

// LevelTrace is a custom log level below DEBUG for very verbose logging
const LevelTrace = slog.Level(-8)

// Trace logs a message at TRACE level
func Trace(msg string, args ...any) {
	slog.Log(nil, LevelTrace, msg, args...)
}
