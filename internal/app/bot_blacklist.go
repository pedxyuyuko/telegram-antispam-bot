package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"telegram-antispam-bot/internal/store"
	"telegram-antispam-bot/internal/telegram"
)

func botLabel(bot store.BotIdentity) string {
	name := strings.TrimSpace(bot.Name)
	if bot.Username != "" {
		name += " @" + bot.Username
	}
	if name == "" {
		return fmt.Sprintf("ID %d", bot.ID)
	}
	return fmt.Sprintf("%s（ID %d）", strings.TrimSpace(name), bot.ID)
}

func (a *App) botBlacklistCommand(ctx context.Context, update int64, m *telegram.Message, name, args string) (string, error) {
	// Keep the global ownership boundary at the command implementation too.
	if m.UserID() == 0 || !a.Config.Snapshot().IsOwner(m.UserID()) {
		return "全局 bot 黑名单由机器人所有者管理。", nil
	}
	fields := strings.Fields(args)
	if name == "as_botlist" {
		offset := 0
		if len(fields) > 1 {
			return "用法：/as_botlist [偏移量]", nil
		}
		if len(fields) == 1 {
			value, err := strconv.Atoi(fields[0])
			if err != nil || value < 0 {
				return "偏移量须为非负整数。", nil
			}
			offset = value
		}
		entries, total, err := a.Store.ListBlockedBots(ctx, offset, 50)
		if err != nil {
			return "", err
		}
		if total == 0 {
			return "全局 bot 黑名单为空。", nil
		}
		lines := []string{fmt.Sprintf("全局 bot 黑名单：%d 个；本页 %d 个，偏移量 %d。", total, len(entries), offset)}
		for _, entry := range entries {
			lines = append(lines, botLabel(entry.BotIdentity))
		}
		if offset+len(entries) < total {
			lines = append(lines, fmt.Sprintf("下一页：/as_botlist %d", offset+50))
		}
		return strings.Join(lines, "\n"), nil
	}
	if len(fields) > 1 {
		return "用法：" + "/" + name + " <bot ID 或 @用户名>；也可回复 bot 消息发送命令。", nil
	}
	var bot store.BotIdentity
	if len(fields) == 0 {
		if m.Reply == nil || m.Reply.BotSenderID() == 0 {
			return "请提供 bot ID、已收录的 @用户名，或回复 bot 消息。", nil
		}
		bot = store.BotIdentity{ID: m.Reply.BotSenderID(), Username: m.Reply.From.Username, Name: strings.TrimSpace(m.Reply.From.FirstName + " " + m.Reply.From.LastName)}
	} else {
		var err error
		bot, err = a.Store.ResolveBot(ctx, fields[0])
		if errors.Is(err, store.ErrUnknownBot) {
			return "用户名尚未收录，请使用 bot ID 或回复 bot 消息。", nil
		}
		if errors.Is(err, store.ErrNotBot) {
			return "这个 ID 的归档身份是普通用户，请选择 bot 账户。", nil
		}
		if err != nil {
			return "", err
		}
	}
	blocked := name == "as_botban"
	if blocked && (bot.ID == a.Telegram.Me.ID || a.Config.Snapshot().IsOwner(bot.ID)) {
		return "当前机器人和所有者身份保持保护。", nil
	}
	change, err := a.Store.SetBotBlocked(ctx, bot, blocked, m.UserID(), update)
	if err != nil {
		return "", err
	}
	if change.Duplicate {
		return "这条黑名单命令已经处理。", nil
	}
	a.diagnostics(m.Chat.ID, "message", m.ID, "update", update).Info("global bot blacklist changed", "event", "blacklist.updated", "bot_id", bot.ID, "actor", m.UserID(), "blocked", blocked, "changed", change.Changed)
	if blocked {
		return "已加入全局 bot 黑名单：" + botLabel(bot) + "。名单按 bot ID 生效，各群继续遵循自己的模式、删除开关和管理员保护。", nil
	}
	return "已移出全局 bot 黑名单：" + botLabel(bot) + "。后续消息恢复模型检测，新的广告删除成功后可再次加入。", nil
}
