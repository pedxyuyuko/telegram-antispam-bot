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
	if reason := a.exemption(ctx, &m, "", diagnostic); reason != "" {
		marked, err := a.Store.IgnoreRevision(ctx, j.ID, reason)
		if err != nil {
			return false, err
		}
		debug(diagnostic, "model.skipped", "reason", reason, "archived_as_ignored", marked)
		return true, nil
	}
	if m.BotSenderID() > 0 {
		blocked, err := a.Store.ApplyBotBlacklist(ctx, *j)
		if err != nil {
			return false, err
		}
		if blocked.Handled {
			debug(diagnostic, "model.skipped", "reason", "bot_blacklist", "bot_id", blocked.BotID, "generation", blocked.Generation)
			for _, action := range blocked.Actions {
				debug(diagnostic, "action.planned", "action_id", action.ID, "action", action.Kind, "rule", action.RuleID, "status", action.Status, "source", "bot_blacklist")
			}
			for _, link := range blocked.LinkedActions {
				debug(diagnostic, "invocation.planned", "target_message", link.MessageID, "target_revision", link.RevisionID, "action_id", link.ActionID, "source_action", link.SourceActionID, "action", link.Kind)
			}
			return true, nil
		}
	}
	cfg := a.Config.Snapshot()
	cached, hit, err := a.Store.ApplyContentCache(ctx, *j, cfg.Model)
	if err != nil {
		return false, err
	}
	if hit {
		if cached.Cache != nil {
			debug(diagnostic, "content_cache.hit", "cache_id", cached.Cache.CacheID, "source_attempt", cached.Cache.SourceAttemptID, "model", cached.Cache.ModelID)
		}
		for _, action := range cached.Actions {
			debug(diagnostic, "action.planned", "action_id", action.ID, "action", action.Kind, "rule", action.RuleID, "status", action.Status, "source", "content_cache")
		}
		for _, link := range cached.LinkedActions {
			debug(diagnostic, "invocation.planned", "target_message", link.MessageID, "target_revision", link.RevisionID, "action_id", link.ActionID, "source_action", link.SourceActionID, "action", link.Kind)
		}
		return true, nil
	}
	reused := len(j.State) > 0
	var inferenceErr error
	if !reused {
		debug(diagnostic, "context.started")
		profile := a.Telegram.Profile(ctx, &m, diagnostic)
		if j.Snapshot.ContextVersion == telegram.SegmentedContextVersion {
			profiles, err := a.referencedProfiles(ctx, *j, &m, profile, diagnostic)
			if err != nil {
				return false, err
			}
			j.State, inferenceErr = m.SegmentedModelState(profile, profiles)
		} else {
			j.State, inferenceErr = m.ModelState(profile)
		}
		if inferenceErr == nil {
			j.State, err = a.Store.FreezeState(ctx, j.ID, j.State)
			if err != nil {
				return false, err
			}
		} else {
			inferenceErr = fmt.Errorf("build model context: %w", inferenceErr)
			debug(diagnostic, "context.failed", "error", inferenceErr)
		}
	}
	var result decision.Result
	if inferenceErr == nil {
		if !hit && j.Snapshot.ContextVersion == telegram.SegmentedContextVersion {
			cached, hit, err = a.Store.ApplyContentCache(ctx, *j, cfg.Model)
			if err != nil {
				return false, err
			}
			if hit {
				if cached.Cache != nil {
					debug(diagnostic, "content_cache.hit", "cache_id", cached.Cache.CacheID, "source_attempt", cached.Cache.SourceAttemptID, "model", cached.Cache.ModelID)
				}
				for _, action := range cached.Actions {
					debug(diagnostic, "action.planned", "action_id", action.ID, "action", action.Kind, "rule", action.RuleID, "status", action.Status, "source", "content_cache")
				}
				for _, link := range cached.LinkedActions {
					debug(diagnostic, "invocation.planned", "target_message", link.MessageID, "target_revision", link.RevisionID, "action_id", link.ActionID, "source_action", link.SourceActionID, "action", link.Kind)
				}
				return true, nil
			}
		}
		debug(diagnostic, "context.ready", "reused", reused, "state_bytes", len(j.State))
		debug(diagnostic, "model.started", "model", cfg.Model.Name, "question_count", len(j.Snapshot.Questions), "questions", questionIDs(j.Snapshot.Questions), "timeout_seconds", cfg.Model.TimeoutSeconds)
		result, inferenceErr = a.Model.Evaluate(ctx, cfg.Model, j.Snapshot.Questions, j.State)
		debug(diagnostic, "model.completed", "success", inferenceErr == nil, "duration_ms", result.Duration.Milliseconds(), "request_bytes", len(result.Request), "response_bytes", len(result.Response))
	} else {
		result.Duration = time.Since(started)
	}
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
		for _, link := range report.LinkedActions {
			debug(diagnostic, "invocation.planned", "target_message", link.MessageID, "target_revision", link.RevisionID, "action_id", link.ActionID, "source_action", link.SourceActionID, "action", link.Kind)
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
		effects, err := a.Store.CompleteAction(persist, intent.ID, status, detail)
		if err != nil {
			diagnostic.Error("save moderation outcome failed", "event", "action.persist_failed", "error", err)
			return true, err
		}
		diagnostic.Info("moderation outcome", "event", "action.completed", "status", status, "detail", detail, "source", intent.EvidenceKind, "duration_ms", time.Since(started).Milliseconds())
		if effects.AddedToBlacklist {
			diagnostic.Info("bot added to global blacklist", "event", "blacklist.added", "bot_id", effects.BotID, "source_action", intent.ID)
		}
		for _, link := range effects.LinkedActions {
			debug(diagnostic, "invocation.planned", "target_message", link.MessageID, "target_revision", link.RevisionID, "action_id", link.ActionID, "source_action", link.SourceActionID, "action", link.Kind)
		}
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
	if valid, reason, err := a.Store.BotActionCurrent(ctx, *intent, cfg); err != nil {
		return false, err
	} else if !valid {
		detail = reason
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
	if message.IsAutomaticForward {
		detail = "automatic channel forward is excluded"
		return finish()
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
		banDeparted := intent.Kind == "ban" && member == "left"
		if intent.Kind != "delete" && member != "member" && member != "restricted" && !banDeparted {
			detail = "target is no longer an active member; preserve existing membership state"
			return finish()
		}
	}
	// Membership checks can wait on the network. Revalidate local authority and
	// evidence immediately before dispatch, after that wait has completed.
	fresh := a.Config.Snapshot()
	freshPolicy := fresh.ForGroup(intent.ChatID)
	freshRule, exists := freshPolicy.Rules[intent.RuleID]
	if freshPolicy.Mode != "enforce" || !exists || !freshRule.Enabled || freshRule != intent.Rule {
		detail = "current policy differs from planned punishment"
		return finish()
	}
	freshQuestions, err := fresh.SelectedQuestions(freshPolicy)
	if err != nil {
		return false, err
	}
	if decision.PolicyKey(job.Snapshot.Policy, job.Snapshot.Questions, intent.Rule) != decision.PolicyKey(freshPolicy, freshQuestions, freshRule) {
		detail = "decision policy or question definitions changed"
		return finish()
	}
	if fresh.IsOwner(intent.UserID) {
		detail = "bot owner is protected"
		return finish()
	}
	current, err = a.Store.ActionCurrent(ctx, *intent)
	if err != nil {
		return false, err
	}
	if !current {
		detail = "message has a newer version"
		return finish()
	}
	release, valid, reason, err := a.Store.GuardBotAction(ctx, *intent, fresh)
	if err != nil {
		return false, err
	}
	if !valid {
		detail = reason
		return finish()
	}
	defer release()
	switch intent.Kind {
	case "ban":
		if intent.Rule.Target != "caller" || intent.EvidenceKind != "bot_invocation" {
			detail = "ban requires confirmed caller evidence"
			return finish()
		}
		var until time.Time
		if intent.Rule.DurationSeconds != -1 {
			if message.Chat.Type != "supergroup" {
				detail = "finite caller ban requires a supergroup"
				return finish()
			}
			until = time.Unix(intent.Until, 0)
			if time.Until(until) < 30*time.Second {
				detail = "ban deadline has expired"
				return finish()
			}
		}
		debug(diagnostic, "action.dispatching")
		err = a.Telegram.Ban(callCtx, intent.ChatID, intent.UserID, until)
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
	errors := make(chan error, 6)
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
	launch("join-model", func(ctx context.Context) (bool, error) { return a.ProcessJoinModel(ctx, time.Now()) })
	launch("join-actions", a.ProcessJoinAction)
	launch("join-service-actions", a.ProcessJoinServiceAction)
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
