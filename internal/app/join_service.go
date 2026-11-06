package app

import (
	"context"
	"encoding/json"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/telegram"
)

func (a *App) ProcessJoinServiceAction(ctx context.Context) (bool, error) {
	intent, err := a.Store.TakeJoinServiceAction(ctx)
	if err != nil || intent == nil {
		return false, err
	}
	status, detail := "skipped", ""
	finish := func() (bool, error) {
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err := a.Store.CompleteJoinServiceAction(persist, intent.ID, status, detail)
		a.Log.Info("join service deletion outcome", "event", "join.service_deleted", "chat", intent.ChatID, "message", intent.MessageID, "action_id", intent.ID, "status", status, "detail", detail, "evidence", intent.Evidence)
		return true, err
	}
	valid, err := a.Store.JoinServiceActionCurrent(ctx, *intent, a.Config.Snapshot())
	if err != nil {
		return false, err
	}
	if !valid {
		detail = "cleanup policy, receipt authority or matched arrival changed"
		return finish()
	}
	j, err := a.Store.Revision(ctx, intent.RevisionID)
	if err != nil {
		return false, err
	}
	var m telegram.Message
	if err := json.Unmarshal(j.Raw, &m); err != nil {
		return false, err
	}
	expired := func() bool { return m.Date <= 0 || time.Now().Unix()-m.Date >= 48*60*60 }
	if expired() {
		detail = "join service message exceeds Telegram deletion window"
		return finish()
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for _, member := range m.NewChatMembers {
		if member.ID == a.Telegram.Me.ID {
			detail = "own bot is protected"
			return finish()
		}
		role, err := a.Telegram.MemberStatus(callCtx, intent.ChatID, member.ID)
		if err != nil {
			detail = "member authority could not be verified: " + err.Error()
			return finish()
		}
		if role != "member" && role != "restricted" && role != "left" && role != "kicked" {
			detail = "service message names a protected or unverifiable member"
			return finish()
		}
	}
	err = a.Config.WithJoinDispatch(func(cfg config.Config) error {
		if expired() {
			detail = "join service message exceeds Telegram deletion window"
			return nil
		}
		ready, err := a.Store.StartJoinServiceAction(ctx, *intent, cfg)
		if err != nil {
			return err
		}
		if !ready {
			detail = "cleanup cancelled or matched evidence changed"
			return nil
		}
		err = a.Telegram.Delete(callCtx, intent.ChatID, intent.MessageID)
		status = "succeeded"
		if err != nil {
			status = "failed"
			detail = err.Error()
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return finish()
}
