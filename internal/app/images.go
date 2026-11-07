package app

import (
	"context"
	"log/slog"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/store"
	"telegram-antispam-bot/internal/telegram"
)

func (a *App) evaluateMessage(ctx context.Context, j store.Job, cfg config.Model, diagnostic *slog.Logger) (decision.Result, error, error) {
	result, err := a.Model.EvaluateWithAssets(ctx, cfg, j.Snapshot.Questions, j.State, j.Assets)
	if len(j.Assets) == 0 || !decision.ImageRejected(err) {
		return result, err, nil
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	attempt, saveErr := a.Store.RecordModelFailure(persist, j.ID, result, err, time.Now())
	cancel()
	if saveErr != nil {
		return decision.Result{}, nil, saveErr
	}
	diagnostic.Warn("image request rejected; retrying text material", "event", "model.image_fallback", "attempt", attempt, "error", err)
	plain, err := telegram.WithoutImages(j.State, j.Assets)
	if err != nil {
		return decision.Result{}, err, nil
	}
	result, err = a.Model.Evaluate(ctx, cfg, j.Snapshot.Questions, plain)
	return result, err, nil
}
