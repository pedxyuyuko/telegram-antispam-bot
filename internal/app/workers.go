package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"telegram-antispam-bot/internal/telegram"
)

func (a *App) ProcessModel(ctx context.Context, now time.Time) (bool, error) {
	j, err := a.Store.Claim(ctx, now)
	if err != nil || j == nil {
		return false, err
	}
	started := time.Now()
	var m telegram.Message
	if err := json.Unmarshal(j.Raw, &m); err != nil {
		return false, fmt.Errorf("read archived message %d: %w", j.ID, err)
	}
	if len(j.State) == 0 {
		j.State = m.State(a.Telegram.Profile(ctx, &m))
		if err := a.Store.SetState(ctx, j.ID, j.State); err != nil {
			return false, err
		}
	}
	cfg := a.Config.Snapshot()
	result, inferenceErr := a.Model.Evaluate(ctx, cfg.Model, j.Snapshot.Questions, j.State)
	// A graceful stop still gets a short opportunity to save the last attempt.
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.Store.Finish(persist, *j, result, inferenceErr, now.Add(time.Since(started)), cfg.Model.RetrySeconds); err != nil {
		return false, err
	}
	if inferenceErr != nil {
		a.Log.Warn("model attempt failed; queued for retry", "chat", j.ChatID, "message", j.MessageID, "revision", j.ID, "error", inferenceErr)
	} else if cfg.ForGroup(j.ChatID).Debug {
		a.Log.Info("model probabilities", "chat", j.ChatID, "message", j.MessageID, "revision", j.ID, "probabilities", result.Probabilities, "duration_ms", result.Duration.Milliseconds())
	}
	return true, nil
}

func (a *App) ProcessAction(ctx context.Context) (bool, error) {
	intent, err := a.Store.TakeAction(ctx)
	if err != nil || intent == nil {
		return false, err
	}
	status, detail := "skipped", ""
	finish := func() (bool, error) {
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err := a.Store.FinishAction(persist, intent.ID, status, detail)
		a.Log.Info("moderation outcome", "chat", intent.ChatID, "message", intent.MessageID, "action", intent.Kind, "rule", intent.RuleID, "status", status, "detail", detail)
		return true, err
	}
	cfg := a.Config.Snapshot()
	policy := cfg.ForGroup(intent.ChatID)
	rule, exists := policy.Rules[intent.RuleID]
	if policy.Mode != "enforce" || !exists || !rule.Enabled || rule != intent.Rule {
		detail = "current policy differs from planned punishment"
		return finish()
	}
	current, err := a.Store.ActionCurrent(ctx, *intent)
	if err != nil {
		return false, err
	}
	if !current {
		detail = "message has a newer version"
		return finish()
	}
	job, err := a.Store.Revision(ctx, intent.RevisionID)
	if err != nil {
		return false, err
	}
	if job.Snapshot.Questions[rule.Question] != cfg.Questions[rule.Question] {
		detail = "decision question changed"
		return finish()
	}
	var message telegram.Message
	if err := json.Unmarshal(job.Raw, &message); err != nil {
		return false, err
	}
	if message.SenderChat != nil && message.SenderChat.ID == message.Chat.ID {
		detail = "anonymous group administrator is protected"
		return finish()
	}
	if intent.UserID == a.Telegram.Me.ID || cfg.IsOwner(intent.UserID) {
		detail = "bot or bot owner is protected"
		return finish()
	}
	if intent.UserID == 0 && intent.Kind != "delete" {
		detail = "sender is a chat; user punishment is inapplicable"
		return finish()
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if intent.UserID != 0 {
		member, err := a.Telegram.MemberStatus(callCtx, intent.ChatID, intent.UserID)
		if err != nil {
			detail = "target membership could not be verified: " + err.Error()
			return finish()
		}
		if member == "administrator" || member == "creator" {
			detail = "group administrator is protected"
			return finish()
		}
		if intent.Kind != "delete" && member != "member" && member != "restricted" {
			detail = "target is no longer an active member; preserve existing membership state"
			return finish()
		}
	}
	switch intent.Kind {
	case "delete":
		err = a.Telegram.Delete(callCtx, intent.ChatID, intent.MessageID)
	case "mute":
		if time.Until(time.Unix(intent.Until, 0)) < 30*time.Second {
			detail = "mute deadline has expired"
			return finish()
		}
		err = a.Telegram.Mute(callCtx, intent.ChatID, intent.UserID, time.Unix(intent.Until, 0))
	case "kick":
		err = a.Telegram.Kick(callCtx, intent.ChatID, intent.UserID)
	default:
		detail = "unsupported action"
		return finish()
	}
	status = "succeeded"
	if err != nil {
		status = "failed"
		detail = err.Error()
	}
	return finish()
}

// Run keeps polling, archive ingestion, model work and action dispatch separate;
// only inference is paused during model outages. Database errors stop the process
// so Telegram cannot silently outrun durable storage.
func (a *App) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errors := make(chan error, 3)
	var wg sync.WaitGroup
	launch := func(name string, step func(context.Context) (bool, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				worked, err := step(ctx)
				if err != nil {
					if ctx.Err() == nil {
						errors <- fmt.Errorf("%s: %w", name, err)
					}
					return
				}
				if !worked {
					timer := time.NewTimer(200 * time.Millisecond)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}
		}()
	}
	launch("inbox", a.ProcessInbox)
	launch("model", func(ctx context.Context) (bool, error) { return a.ProcessModel(ctx, time.Now()) })
	launch("actions", a.ProcessAction)
	wg.Add(1)
	go func() { defer wg.Done(); a.Telegram.Start(ctx) }()
	var err error
	select {
	case <-ctx.Done():
	case err = <-errors:
	}
	cancel()
	wg.Wait()
	return err
}
