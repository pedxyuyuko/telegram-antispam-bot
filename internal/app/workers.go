package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/telegram"
)

func (a *App) ProcessModel(ctx context.Context, now time.Time) (bool, error) {
	j, err := a.Store.Claim(ctx, now)
	if err != nil || j == nil {
		return false, err
	}
	started := time.Now()
	diagnostic := a.diagnostics(j.ChatID, "message", j.MessageID, "revision", j.ID, "update", j.UpdateID)
	debug(diagnostic, "model.claimed", "mode", j.Snapshot.Policy.Mode, "queue_wait_ms", max(int64(0), now.UnixMilli()-j.ReceivedAt))
	var m telegram.Message
	if err := json.Unmarshal(j.Raw, &m); err != nil {
		return false, fmt.Errorf("read archived message %d: %w", j.ID, err)
	}
	reused := len(j.State) > 0
	if !reused {
		debug(diagnostic, "context.started")
		j.State = m.State(a.Telegram.Profile(ctx, &m, diagnostic))
		if err := a.Store.SetState(ctx, j.ID, j.State); err != nil {
			return false, err
		}
	}
	debug(diagnostic, "context.ready", "reused", reused, "state_bytes", len(j.State))
	cfg := a.Config.Snapshot()
	debug(diagnostic, "model.started", "model", cfg.Model.Name, "question_count", len(j.Snapshot.Questions), "questions", questionIDs(j.Snapshot.Questions), "timeout_seconds", cfg.Model.TimeoutSeconds)
	result, inferenceErr := a.Model.Evaluate(ctx, cfg.Model, j.Snapshot.Questions, j.State)
	debug(diagnostic, "model.completed", "success", inferenceErr == nil, "duration_ms", result.Duration.Milliseconds(), "request_bytes", len(result.Request), "response_bytes", len(result.Response))
	// A graceful stop still gets a short opportunity to save the last attempt.
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	report, err := a.Store.Finish(persist, *j, result, inferenceErr, now.Add(time.Since(started)), cfg.Model.RetrySeconds)
	if err != nil {
		return false, err
	}
	diagnostic = diagnostic.With("attempt", report.AttemptID)
	debug(diagnostic, "model.archived", "status", report.Status, "reason", report.Reason, "mode", j.Snapshot.Policy.Mode, "duration_ms", time.Since(started).Milliseconds())
	if inferenceErr != nil {
		diagnostic.Warn("model attempt failed", "event", "model.failed", "status", report.Status, "error", inferenceErr, "retry_at", time.UnixMilli(report.RetryAt))
		debug(diagnostic, "model.retry_scheduled", "status", report.Status, "retry_at", time.UnixMilli(report.RetryAt), "retry_seconds", cfg.Model.RetrySeconds, "scope", "model")
	} else {
		debug(diagnostic, "model.answers", "probabilities", result.Probabilities, "answers", answerSummaries(j.Snapshot.Questions, report.Evaluation.Answers))
		debug(diagnostic, "policy.scored", "score", report.Evaluation.Score, "condition", j.Snapshot.Policy.Condition, "condition_result", report.Evaluation.Condition, "terms", report.Evaluation.Terms)
		debug(diagnostic, "policy.rules", "reason", report.Reason, "rules", report.Rules)
		for _, action := range report.Actions {
			debug(diagnostic, "action.planned", "action_id", action.ID, "action", action.Kind, "rule", action.RuleID, "status", action.Status, "until", action.Until, "mode", j.Snapshot.Policy.Mode)
		}
	}
	return true, nil
}

func (a *App) ProcessAction(ctx context.Context) (bool, error) {
	intent, err := a.Store.TakeAction(ctx)
	if err != nil || intent == nil {
		return false, err
	}
	started := time.Now()
	diagnostic := a.diagnostics(intent.ChatID, "message", intent.MessageID, "revision", intent.RevisionID, "action_id", intent.ID, "action", intent.Kind, "rule", intent.RuleID)
	debug(diagnostic, "action.preflight", "until", intent.Until)
	status, detail := "skipped", ""
	finish := func() (bool, error) {
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err := a.Store.FinishAction(persist, intent.ID, status, detail)
		if err != nil {
			diagnostic.Error("save moderation outcome failed", "event", "action.persist_failed", "error", err)
			return true, err
		}
		diagnostic.Info("moderation outcome", "event", "action.completed", "status", status, "detail", detail, "duration_ms", time.Since(started).Milliseconds())
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
	questions, err := cfg.SelectedQuestions(policy)
	if err != nil {
		return false, err
	}
	if decision.PolicyKey(job.Snapshot.Policy, job.Snapshot.Questions, intent.Rule) != decision.PolicyKey(policy, questions, rule) {
		detail = "decision policy or question definitions changed"
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
		debug(diagnostic, "action.membership", "membership", member)
		if intent.Kind != "delete" && member != "member" && member != "restricted" {
			detail = "target is no longer an active member; preserve existing membership state"
			return finish()
		}
	}
	switch intent.Kind {
	case "delete":
		debug(diagnostic, "action.dispatching")
		err = a.Telegram.Delete(callCtx, intent.ChatID, intent.MessageID)
	case "mute":
		if time.Until(time.Unix(intent.Until, 0)) < 30*time.Second {
			detail = "mute deadline has expired"
			return finish()
		}
		debug(diagnostic, "action.dispatching")
		err = a.Telegram.Mute(callCtx, intent.ChatID, intent.UserID, time.Unix(intent.Until, 0))
	case "kick":
		debug(diagnostic, "action.dispatching")
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
