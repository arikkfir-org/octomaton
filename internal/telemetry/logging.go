package telemetry

import (
	"context"
	"io"
	"log/slog"
	"strconv"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"k8s.io/klog/v2"
)

func newLogHandler(c Config, w io.Writer) slog.Handler {
	if c.LogFormat == "text" {
		return slog.NewTextHandler(w, &slog.HandlerOptions{Level: c.LogLevel})
	}
	return slog.NewJSONHandler(w, &slog.HandlerOptions{Level: c.LogLevel, AddSource: true, ReplaceAttr: cloudLoggingAttr})
}

// cloudLoggingAttr renames slog's built-in attributes to the fields Cloud Logging reads from JSON
// logs, so that entries get their severity, message, time and source location.
func cloudLoggingAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.TimeKey:
		a.Key = "timestamp"
	case slog.MessageKey:
		a.Key = "message"
	case slog.LevelKey:
		level, _ := a.Value.Any().(slog.Level)
		return slog.String("severity", severity(level))
	case slog.SourceKey:
		if src, ok := a.Value.Any().(*slog.Source); ok {
			return slog.Group("logging.googleapis.com/sourceLocation",
				slog.String("file", src.File), slog.String("line", strconv.Itoa(src.Line)), slog.String("function", src.Function))
		}
	}
	return a
}

func severity(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return "DEBUG"
	case level < slog.LevelWarn:
		return "INFO"
	case level < slog.LevelError:
		return "WARNING"
	default:
		return "ERROR"
	}
}

// installLogger makes h the handler of slog's default logger, of client-go (klog) and of
// OpenTelemetry's own diagnostics.
func installLogger(h slog.Handler) {
	logger := slog.New(h)
	slog.SetDefault(logger)
	klog.SetSlogLogger(logger.With("component", "client-go"))
	// OpenTelemetry logs its routine diagnostics at logr verbosity 1 and up (below slog's info).
	otel.SetLogger(logr.FromSlogHandler(minLevel{Handler: h, level: slog.LevelInfo}))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("OpenTelemetry error", "error", err)
	}))
}

// minLevel passes records at or above level to the wrapped handler.
type minLevel struct {
	slog.Handler
	level slog.Level
}

func (h minLevel) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level && h.Handler.Enabled(ctx, level)
}

func (h minLevel) WithAttrs(attrs []slog.Attr) slog.Handler {
	return minLevel{Handler: h.Handler.WithAttrs(attrs), level: h.level}
}

func (h minLevel) WithGroup(name string) slog.Handler {
	return minLevel{Handler: h.Handler.WithGroup(name), level: h.level}
}
