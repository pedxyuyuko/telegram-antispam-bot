package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/store"
	"telegram-antispam-bot/internal/telegram"
)

func (a *App) joinDiagnostics(chat int64, fields ...any) *slog.Logger {
	h := debugHandler{a.Log.Handler(), func() bool { return a.Config.Snapshot().ForJoinGroup(chat).Debug }}
	return slog.New(h).With("chat", chat, "scope", "join_gate").With(fields...)
}

func (a *App) processMembership(ctx context.Context, in store.Inbox, m telegram.MembershipUpdate) (bool, error) {
	if !m.Valid() {
		return true, a.Store.MarkInbox(ctx, in.ID, "invalid or non-group membership event")
	}
	cfg := a.Config.Snapshot()
	p := cfg.ForJoinGroup(m.Chat.ID)
	var questions map[string]config.Question
	if cfg.JoinGate != nil {
		var err error
		questions, err = cfg.SelectedQuestions(p.Policy)
		if err != nil {
			return false, err
		}
	}
	skip := ""
	if m.New.User.ID == a.Telegram.Me.ID {
		skip = "own_bot"
	} else if cfg.IsOwner(m.New.User.ID) {
		skip = "bot_owner"
	} else if groupAdministrator(m.New.Status) {
		skip = "group_administrator"
	}
	id, err := a.Store.AddJoinEvent(ctx, in, m, store.JoinSnapshot{Policy: p, Questions: questions, ModelID: cfg.Model.Name, ModelEndpoint: cfg.Model.BaseURL}, skip)
	if err != nil {
		return false, err
	}
	debug(a.joinDiagnostics(m.Chat.ID, "join", id, "update", in.ID, "user", m.New.User.ID), "join.received", "arrival", m.Joined(), "enabled", p.Enabled, "mode", p.Mode, "skip_reason", skip)
	return true, nil
}

func (a *App) ProcessJoinModel(ctx context.Context, now time.Time) (bool, error) {
	j, err := a.Store.ClaimJoin(ctx, now)
	if err != nil || j == nil {
		return false, err
	}
	started := time.Now()
	cfg := a.Config.Snapshot()
	diagnostic := a.joinDiagnostics(j.ChatID, "join", j.ID, "user", j.UserID)
	skip := ""
	if !cfg.ForJoinGroup(j.ChatID).Enabled {
		skip = "join gate disabled"
	} else if cfg.IsOwner(j.UserID) || j.UserID == a.Telegram.Me.ID {
		skip = "protected identity"
	}
	if skip == "" {
		current, err := a.Store.JoinCurrent(ctx, j.ID)
		if err != nil {
			return false, err
		}
		if !current {
			skip = "membership episode changed"
		}
	}
	if skip == "" {
		m := &telegram.Message{Chat: j.Event.Chat, From: &j.Event.New.User}
		role := a.membership(ctx, m, diagnostic)
		if groupAdministrator(role) {
			skip = "group administrator"
		} else if role == "left" || role == "kicked" {
			skip = "member already departed"
		}
	}
	if skip != "" {
		return true, a.Store.IgnoreJoin(ctx, j.ID, skip)
	}
	var inferenceErr error
	var result decision.Result
	if len(j.State) == 0 {
		profile := a.Telegram.JoinProfile(ctx, j.UserID)
		j.State, inferenceErr = telegram.JoinModelState(j.Event.New.User, profile)
		if inferenceErr == nil {
			persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			j.State, err = a.Store.FreezeJoinContext(persist, j.ID, profile, j.State)
			cancel()
			if err != nil {
				return false, err
			}
		}
		debug(diagnostic, "join.profile", "status", profile.Status, "duration_ms", profile.DurationMS)
	}
	if inferenceErr == nil {
		result, inferenceErr = a.Model.Evaluate(ctx, cfg.Model, j.Snapshot.Questions, j.State)
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	report, err := a.Store.FinishJoin(persist, *j, result, inferenceErr, now.Add(time.Since(started)), cfg.Model.RetrySeconds)
	if err != nil {
		return false, err
	}
	if inferenceErr != nil {
		diagnostic.Warn("join model attempt failed", "event", "join.model_failed", "attempt", report.AttemptID, "error", inferenceErr, "retry_at", report.RetryAt)
	} else {
		diagnostic.Info("join detection recorded", "event", "join.evaluated", "attempt", report.AttemptID, "status", report.Status, "score", report.Evaluation.Score, "condition", report.Evaluation.Condition, "mode", j.Snapshot.Policy.Mode)
		debug(diagnostic, "join.answers", "answers", answerSummaries(j.Snapshot.Questions, report.Evaluation.Answers), "terms", report.Evaluation.Terms)
	}
	return true, nil
}

func (a *App) joinActionPolicy(cfg config.Config, j *store.JoinJob, intent *store.JoinAction) (bool, error) {
	p := cfg.ForJoinGroup(j.ChatID)
	rule, exists := p.Rules[intent.RuleID]
	if !p.Enabled || p.Mode != "enforce" || !j.Snapshot.Policy.Enabled || j.Snapshot.Policy.Mode != "enforce" || !exists || !rule.Enabled || rule != intent.Rule || cfg.IsOwner(j.UserID) || j.UserID == a.Telegram.Me.ID {
		return false, nil
	}
	questions, err := cfg.SelectedQuestions(p.Policy)
	if err != nil {
		return false, err
	}
	return decision.PolicyKey(p.Policy, questions, rule) == decision.PolicyKey(j.Snapshot.Policy.Policy, j.Snapshot.Questions, intent.Rule), nil
}

func (a *App) ProcessJoinAction(ctx context.Context) (bool, error) {
	intent, err := a.Store.TakeJoinAction(ctx)
	if err != nil || intent == nil {
		return false, err
	}
	status, detail := "skipped", ""
	diagnostic := a.joinDiagnostics(intent.ChatID, "join", intent.JoinID, "user", intent.UserID, "action", intent.ID)
	finish := func() (bool, error) {
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err := a.Store.CompleteJoinAction(persist, intent.ID, status, detail)
		diagnostic.Info("join moderation outcome", "event", "join.action_completed", "status", status, "detail", detail)
		return true, err
	}
	j, err := a.Store.JoinEvent(ctx, intent.JoinID)
	if err != nil {
		return false, err
	}
	if valid, err := a.joinActionPolicy(a.Config.Snapshot(), j, intent); err != nil {
		return false, err
	} else if !valid {
		detail = "join policy or protected identity changed"
		return finish()
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	role, err := a.Telegram.MemberStatus(callCtx, intent.ChatID, intent.UserID)
	if err != nil {
		detail = "target membership could not be verified: " + err.Error()
		return finish()
	}
	if role != "member" && role != "restricted" {
		detail = "target is protected or no longer an active member: " + role
		return finish()
	}
	// Network preflight can outlive a command or a new membership update.
	err = a.Config.WithJoinDispatch(func(cfg config.Config) error {
		valid, err := a.joinActionPolicy(cfg, j, intent)
		if err != nil {
			return err
		}
		if !valid {
			detail = "join policy or protected identity changed"
			return nil
		}
		ready, err := a.Store.StartJoinAction(ctx, *intent)
		if err != nil {
			return err
		}
		if !ready {
			detail = "join action cancelled or membership episode changed"
			return nil
		}
		err = a.Telegram.Kick(callCtx, intent.ChatID, intent.UserID)
		status = "succeeded"
		if err != nil {
			status = "failed"
			detail = fmt.Sprint(err)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return finish()
}
