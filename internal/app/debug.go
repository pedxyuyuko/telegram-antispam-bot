package app

import (
	"context"
	"log/slog"
	"sort"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
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

// Project validated fields by requested type. Raw answers may contain extra
// model-controlled text in fields belonging to another type; keep that in SQLite.
func answerSummaries(questions map[string]config.Question, answers map[string]decision.EffectiveAnswer) map[string]any {
	out := make(map[string]any, len(questions))
	for id, q := range questions {
		a := answers[id]
		summary := map[string]any{"type": q.Type, "status": a.Status}
		if a.Reason != "" {
			summary["reason"] = a.Reason
		}
		switch q.Type {
		case "noul":
			summary["noul"] = a.Raw.Noul
		case "choice":
			summary["choice"] = a.Raw.Choice
			summary["probabilities"] = a.Raw.Probabilities
			summary["confidence"] = a.Raw.Confidence
		case "score":
			summary["score"] = a.Raw.Score
			summary["probabilities"] = a.Raw.Probabilities
			summary["confidence"] = a.Raw.Confidence
		}
		out[id] = summary
	}
	return out
}

func (a *App) resizeQueues(ctx context.Context, update, sourceChat int64) error {
	changes, err := a.Store.Resize(ctx, a.Config.Snapshot())
	if err != nil {
		return err
	}
	for _, change := range changes {
		debug(a.diagnostics(change.ChatID, "update", update, "source_chat", sourceChat), "queue.resized", "queue_limit", change.Limit, "evicted", change.Evicted, "reason", "configuration_changed")
	}
	return nil
}
