package battery

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

type LogLevel int

const (
	LogLevelNone LogLevel = iota
	LogLevelError
	LogLevelWarning
	LogLevelInfo
	LogLevelDebug
)

// FSMHandler is a custom slog.Handler for FSM logs
type FSMHandler struct {
	w         io.Writer
	level     LogLevel
	attrs     []slog.Attr
	groups    []string
	mu        sync.Mutex
	batteryID int
}

// NewFSMHandler creates a new FSM handler with the specified log level and battery ID
func NewFSMHandler(w io.Writer, level LogLevel, batteryID int) *FSMHandler {
	return &FSMHandler{
		w:         w,
		level:     level,
		batteryID: batteryID,
	}
}

// Enabled reports whether the handler handles records at the given level
func (h *FSMHandler) Enabled(_ context.Context, level slog.Level) bool {
	switch level {
	case slog.LevelDebug:
		return h.level >= LogLevelDebug
	case slog.LevelInfo:
		return h.level >= LogLevelInfo
	case slog.LevelWarn:
		return h.level >= LogLevelWarning
	case slog.LevelError:
		return h.level >= LogLevelError
	default:
		return false
	}
}

// Handle formats and writes a log record
func (h *FSMHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Map slog level to our prefix
	var prefix string
	switch r.Level {
	case slog.LevelDebug:
		prefix = "DEBUG"
	case slog.LevelInfo:
		prefix = ""
	case slog.LevelWarn:
		prefix = "WARN"
	case slog.LevelError:
		prefix = "ERROR"
	}

	// Build the message
	buf := make([]byte, 0, 256)

	// Add battery ID and prefix
	buf = fmt.Appendf(buf, "Battery %d: ", h.batteryID)
	if prefix != "" {
		buf = fmt.Appendf(buf, "%s: ", prefix)
	}

	// Add the main message
	buf = append(buf, r.Message...)

	// Add attributes as key=value pairs (only for non-empty messages with attrs)
	if r.NumAttrs() > 0 {
		// Check if message ends with colon (like "entering state:")
		if len(buf) > 0 && buf[len(buf)-1] != ':' {
			buf = append(buf, ':')
		}

		first := true
		r.Attrs(func(a slog.Attr) bool {
			// Skip the "battery" attribute as we already included it in the prefix
			if a.Key == "battery" {
				return true
			}

			if !first {
				buf = append(buf, ',')
			}
			buf = append(buf, ' ')
			buf = append(buf, a.Key...)
			buf = append(buf, '=')
			buf = append(buf, a.Value.String()...)
			first = false
			return true
		})
	}

	buf = append(buf, '\n')
	_, err := h.w.Write(buf)
	return err
}

// WithAttrs returns a new handler with the given attributes added
func (h *FSMHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)

	return &FSMHandler{
		w:         h.w,
		level:     h.level,
		attrs:     newAttrs,
		groups:    h.groups,
		batteryID: h.batteryID,
	}
}

// WithGroup returns a new handler with the given group added
func (h *FSMHandler) WithGroup(name string) slog.Handler {
	newGroups := make([]string, len(h.groups)+1)
	copy(newGroups, h.groups)
	newGroups[len(h.groups)] = name

	return &FSMHandler{
		w:         h.w,
		level:     h.level,
		attrs:     h.attrs,
		groups:    newGroups,
		batteryID: h.batteryID,
	}
}

// ServiceHandler is a custom slog.Handler for service-level logs
type ServiceHandler struct {
	w      io.Writer
	level  LogLevel
	attrs  []slog.Attr
	groups []string
	mu     sync.Mutex
}

// NewServiceHandler creates a new service handler with the specified log level
func NewServiceHandler(w io.Writer, level LogLevel) *ServiceHandler {
	return &ServiceHandler{
		w:     w,
		level: level,
	}
}

// Enabled reports whether the handler handles records at the given level
func (h *ServiceHandler) Enabled(_ context.Context, level slog.Level) bool {
	switch level {
	case slog.LevelDebug:
		return h.level >= LogLevelDebug
	case slog.LevelInfo:
		return h.level >= LogLevelInfo
	case slog.LevelWarn:
		return h.level >= LogLevelWarning
	case slog.LevelError:
		return h.level >= LogLevelError
	default:
		return false
	}
}

// Handle formats and writes a log record
func (h *ServiceHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Map slog level to our prefix
	var prefix string
	switch r.Level {
	case slog.LevelDebug:
		prefix = "DEBUG"
	case slog.LevelInfo:
		prefix = ""
	case slog.LevelWarn:
		prefix = "WARN"
	case slog.LevelError:
		prefix = "ERROR"
	}

	// Build the message
	buf := make([]byte, 0, 256)

	// Add prefix
	if prefix != "" {
		buf = fmt.Appendf(buf, "%s: ", prefix)
	}

	// Add the main message
	buf = append(buf, r.Message...)

	buf = append(buf, '\n')
	_, err := h.w.Write(buf)
	return err
}

// WithAttrs returns a new handler with the given attributes added
func (h *ServiceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)

	return &ServiceHandler{
		w:      h.w,
		level:  h.level,
		attrs:  newAttrs,
		groups: h.groups,
	}
}

// WithGroup returns a new handler with the given group added
func (h *ServiceHandler) WithGroup(name string) slog.Handler {
	newGroups := make([]string, len(h.groups)+1)
	copy(newGroups, h.groups)
	newGroups[len(h.groups)] = name

	return &ServiceHandler{
		w:      h.w,
		level:  h.level,
		attrs:  h.attrs,
		groups: newGroups,
	}
}
