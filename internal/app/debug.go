package app

import (
	"context"
	"log/slog"
	"sort"

	"telegram-antispam-bot/internal/config"
)

// Gate every diagnostic event against the live group setting, including profile
// events emitted during a request. Handler clones retain the same live gate.
type debugHandler struct {
	slog.Handler
	enabled func() bool
}

func (h debugHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Handler.Enabled(ctx, level) && (level >= slog.LevelInfo || h.enabled())
}
func (h debugHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return debugHandler{h.Handler.WithAttrs(attrs), h.enabled}
}
func (h debugHandler) WithGroup(name string) slog.Handler {
	return debugHandler{h.Handler.WithGroup(name), h.enabled}
}

func (a *App) diagnostics(chat int64, fields ...any) *slog.Logger {
	h := debugHandler{a.Log.Handler(), func() bool { return a.Config.Snapshot().ForGroup(chat).Debug }}
	return slog.New(h).With("chat", chat).With(fields...)
}

func debug(log *slog.Logger, event string, fields ...any) {
	log.Debug(event, append([]any{"event", event}, fields...)...)
}

func questionIDs(questions map[string]config.Question) []string {
	ids := make([]string, 0, len(questions))
	for id := range questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
