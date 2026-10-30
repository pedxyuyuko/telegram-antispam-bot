package app

import (
	"context"
	"log/slog"
	"time"

	"telegram-antispam-bot/internal/telegram"
)

func groupAdministrator(status string) bool { return status == "administrator" || status == "creator" }

func (a *App) membership(ctx context.Context, m *telegram.Message, diagnostic *slog.Logger) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	started := time.Now()
	status, err := a.Telegram.MemberStatus(ctx, m.Chat.ID, m.UserID())
	if err != nil {
		diagnostic.Warn("group membership lookup failed", "event", "membership.failed", "user", m.UserID(), "error", err)
		return "unavailable"
	}
	debug(diagnostic, "membership.checked", "user", m.UserID(), "membership", status, "duration_ms", time.Since(started).Milliseconds())
	return status
}

// A previously checked role can be reused for a command's admission decision.
// Unknown roles remain eligible for judgment; moderation still verifies them.
func (a *App) exemption(ctx context.Context, m *telegram.Message, checkedRole string, diagnostic *slog.Logger) string {
	if !m.IsGroup() {
		return "non_group"
	}
	if m.UserID() == a.Telegram.Me.ID {
		return "own_bot"
	}
	if m.IsAutomaticForward {
		return "automatic_channel_forward"
	}
	if m.SenderChat != nil && m.SenderChat.ID == m.Chat.ID {
		return "anonymous_administrator"
	}
	if m.UserID() != 0 {
		if checkedRole == "" {
			checkedRole = a.membership(ctx, m, diagnostic)
		}
		if groupAdministrator(checkedRole) {
			return "group_administrator"
		}
	}
	return ""
}
