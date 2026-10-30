package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/store"
	"telegram-antispam-bot/internal/telegram"
)

type App struct {
	Config   *config.Manager
	Store    *store.Store
	Telegram *telegram.Client
	Model    *decision.Client
	Log      *slog.Logger
}

var commands = map[string]bool{"as_help": true, "start": true, "as_config": true, "as_set": true, "as_reset": true, "as_reload": true, "as_status": true, "as_spam": true, "as_ham": true}

func (a *App) identify(m *telegram.Message) (name, args string) {
	text := strings.TrimSpace(m.Text)
	if strings.HasPrefix(text, "/") {
		first, rest, _ := strings.Cut(text, " ")
		// Fields also handles commands followed by a newline or tab.
		fields := strings.Fields(text)
		if len(fields) > 0 {
			first = fields[0]
			rest = strings.TrimSpace(text[len(first):])
		}
		command, username, addressed := strings.Cut(strings.TrimPrefix(first, "/"), "@")
		if addressed && !strings.EqualFold(username, a.Telegram.Me.Username) {
			return "", ""
		}
		if commands[command] {
			return command, strings.TrimSpace(rest)
		}
	}
	if m.Reply != nil {
		for _, part := range []struct {
			text     string
			entities []telegram.Entity
		}{{m.Text, m.Entities}, {m.Caption, m.CaptionEntities}} {
			for _, e := range part.entities {
				if e.Type == "mention" && strings.EqualFold(telegram.EntityText(part.text, e), "@"+a.Telegram.Me.Username) {
					return "as_spam", ""
				}
				if e.Type == "text_mention" && e.User != nil && e.User.ID == a.Telegram.Me.ID {
					return "as_spam", ""
				}
			}
		}
	}
	return "", ""
}

func sharedCommand(name, args string) bool {
	return name == "as_reload" || (name == "as_config" && args == "base") || (name == "as_set" && len(strings.Fields(args)) > 0 && strings.Fields(args)[0] == "base")
}

func (a *App) authorized(ctx context.Context, m *telegram.Message, name, args string, diagnostic *slog.Logger) (bool, string) {
	user := m.UserID()
	if user == 0 {
		return false, ""
	}
	if sharedCommand(name, args) {
		return a.Config.Snapshot().IsOwner(user), ""
	}
	if !m.IsGroup() {
		return name == "as_help" || name == "start", ""
	}
	status := a.membership(ctx, m, diagnostic)
	return groupAdministrator(status), status
}

func (a *App) ProcessInbox(ctx context.Context) (bool, error) {
	in, err := a.Store.NextInbox(ctx)
	if err != nil || in == nil {
		return false, err
	}
	u, err := telegram.Parse(in.Raw)
	if err != nil {
		debug(a.diagnostics(0, "update", in.ID), "update.ignored", "reason", "invalid_update")
		return true, a.Store.MarkInbox(ctx, in.ID, "invalid update: "+err.Error())
	}
	m := u.Current()
	if m == nil {
		debug(a.diagnostics(0, "update", in.ID), "update.ignored", "reason", "unsupported_update")
		return true, a.Store.MarkInbox(ctx, in.ID, "unsupported update type")
	}
	if m.ID <= 0 || m.Chat.ID == 0 {
		debug(a.diagnostics(m.Chat.ID, "update", in.ID), "update.ignored", "reason", "invalid_message_identity")
		return true, a.Store.MarkInbox(ctx, in.ID, "invalid message identity")
	}
	name, args := a.identify(m)
	diagnostic := a.diagnostics(m.Chat.ID, "message", m.ID, "update", in.ID)
	debug(diagnostic, "message.received", "sender", m.SenderKey(), "sender_is_bot", m.From != nil && m.From.IsBot, "automatic_forward", m.IsAutomaticForward, "edited", u.Edited != nil, "command", name, "text_bytes", len(m.Text), "caption_bytes", len(m.Caption), "has_reply", m.Reply != nil)
	var authorized bool
	var checkedRole string
	if name != "" {
		authorized, checkedRole = a.authorized(ctx, m, name, args, diagnostic)
	}
	if name != "" {
		debug(diagnostic, "command.authorization", "command", name, "allowed", authorized)
	}
	status, skipReason := "pending", ""
	if authorized {
		status = "command"
	} else if skipReason = a.exemption(ctx, m, checkedRole, diagnostic); skipReason != "" {
		status = "ignored"
	}
	cfg := a.Config.Snapshot()
	policy := cfg.ForGroup(m.Chat.ID)
	questions, err := cfg.SelectedQuestions(policy)
	if err != nil {
		return false, err
	}
	archived, err := a.Store.AddRevision(ctx, *in, m, store.Snapshot{Policy: policy, Questions: questions, SkipReason: skipReason}, status)
	if err != nil {
		return false, err
	}
	diagnostic = diagnostic.With("revision", archived.ID)
	debug(diagnostic, "message.archived", "status", archived.Status, "skip_reason", skipReason, "duplicate", archived.Duplicate, "mode", policy.Mode, "question_count", len(questions), "queue_limit", policy.QueueLimit)
	if archived.Evicted > 0 {
		debug(diagnostic, "queue.evicted", "evicted", archived.Evicted, "queue_limit", policy.QueueLimit, "reason", "new_message")
	}
	if authorized {
		reply, err := a.command(ctx, in.ID, m, name, args)
		if err != nil {
			return false, err
		}
		if reply != "" {
			if err := a.Telegram.Send(ctx, m, reply); err != nil {
				a.Log.Warn("command reply failed", "chat", m.Chat.ID, "error", err)
			}
		}
	} else if name != "" {
		a.Log.Info("unauthorized command ignored", "chat", m.Chat.ID, "user", m.UserID(), "command", name)
	}
	return true, a.Store.MarkInbox(ctx, in.ID, "")
}

const help = `群管理员命令：
/as_config — 查看有效配置和本群覆盖项
/as_set <参数路径> <JSON值> — 修改本群覆盖
/as_reset <参数路径> — 恢复继承
/as_status — 查看本群处理统计
/as_spam 或 /as_ham — 回复消息进行人工标注
/as_spam <消息ID> 或 /as_ham <消息ID> — 标注已入库的消息
回复原消息并 @本机器人也会标注为 spam。

机器人所有者命令：
/as_config base — 查看共享配置
/as_set base <参数路径> <JSON值> — 修改共享配置
/as_reload — 校验并重载服务器配置文件

示例：
/as_set mode "record"
/as_set queue_limit 100
/as_set rules.repeat.duration_seconds 120
/as_set rules.repeat.enabled false`

func (a *App) command(ctx context.Context, updateID int64, m *telegram.Message, name, args string) (string, error) {
	diagnostic := a.diagnostics(m.Chat.ID, "message", m.ID, "update", updateID, "command", name, "actor", m.UserID())
	switch name {
	case "start", "as_help":
		return help, nil
	case "as_config":
		c := a.Config.Snapshot()
		var value any
		if args == "base" {
			value = c.Base
		} else {
			if !m.IsGroup() {
				return "请在目标群里查看群配置，或使用 /as_config base。", nil
			}
			overrides := c.Groups[strconv.FormatInt(m.Chat.ID, 10)]
			if overrides == nil {
				overrides = json.RawMessage(`{}`)
			}
			value = struct {
				Effective config.Policy   `json:"effective"`
				Overrides json.RawMessage `json:"group_overrides"`
			}{c.ForGroup(m.Chat.ID), overrides}
		}
		raw, _ := json.MarshalIndent(value, "", "  ")
		return string(raw), nil
	case "as_set":
		var chat *int64
		if sharedCommand(name, args) {
			args = strings.TrimSpace(strings.TrimPrefix(args, "base"))
		} else {
			if !m.IsGroup() {
				return "本群配置需要在群内修改。共享配置使用 /as_set base。", nil
			}
			id := m.Chat.ID
			chat = &id
		}
		fields := strings.Fields(args)
		if len(fields) < 2 {
			debug(diagnostic, "config.rejected", "reason", "missing_arguments")
			return "用法：/as_set [base] <参数路径> <JSON值>", nil
		}
		path := fields[0]
		value := strings.TrimSpace(args[len(path):])
		if !json.Valid([]byte(value)) {
			debug(diagnostic, "config.rejected", "reason", "invalid_json")
			return "值需要使用 JSON 格式，例如 true、100 或 \"record\"。", nil
		}
		if err := a.Config.Set(chat, path, json.RawMessage(value)); err != nil {
			debug(diagnostic, "config.rejected", "reason", "validation_or_write_failed")
			return "配置修改失败：" + err.Error(), nil
		}
		scope := "group"
		if chat == nil {
			scope = "base"
		}
		diagnostic.Info("configuration updated", "event", "config.updated", "scope", scope, "setting", path)
		if err := a.resizeQueues(ctx, updateID, m.Chat.ID); err != nil {
			return "", err
		}
		return "配置已生效并写回文件。", nil
	case "as_reset":
		if !m.IsGroup() {
			return "请在目标群里恢复配置继承。", nil
		}
		if err := a.Config.Reset(m.Chat.ID, strings.TrimSpace(args)); err != nil {
			debug(diagnostic, "config.rejected", "reason", "reset_failed")
			return "恢复失败：" + err.Error(), nil
		}
		diagnostic.Info("configuration inheritance restored", "event", "config.reset", "scope", "group", "setting", strings.TrimSpace(args))
		if err := a.resizeQueues(ctx, updateID, m.Chat.ID); err != nil {
			return "", err
		}
		return "该参数已恢复继承共享配置。", nil
	case "as_reload":
		if err := a.Config.Reload(); err != nil {
			debug(diagnostic, "config.rejected", "reason", "reload_failed")
			return "重载失败，继续使用当前配置：" + err.Error(), nil
		}
		diagnostic.Info("configuration reloaded", "event", "config.reloaded", "scope", "base", "group_overrides", len(a.Config.Snapshot().Groups))
		if err := a.resizeQueues(ctx, updateID, m.Chat.ID); err != nil {
			return "", err
		}
		return "文件配置已校验并生效。", nil
	case "as_status":
		if !m.IsGroup() {
			return "请在目标群里查看处理统计。", nil
		}
		stats, err := a.Store.Stats(ctx, m.Chat.ID)
		if err != nil {
			return "", err
		}
		raw, _ := json.MarshalIndent(stats, "", "  ")
		return string(raw), nil
	case "as_spam", "as_ham":
		if !m.IsGroup() {
			return "请在原消息所在群里进行标注。", nil
		}
		var target *telegram.Message
		if args == "" {
			target = m.Reply
		} else {
			id, err := strconv.Atoi(strings.TrimSpace(args))
			if err != nil || id <= 0 {
				return "请回复原消息，或提供本群的消息 ID。", nil
			}
			raw, err := a.Store.LatestMessage(ctx, m.Chat.ID, id)
			if err != nil {
				return "数据库中找不到这条群消息。", nil
			}
			target = &telegram.Message{}
			if err := json.Unmarshal(raw, target); err != nil {
				return "", err
			}
		}
		if target == nil || target.Chat.ID != m.Chat.ID {
			return "请回复本群的原消息，或提供本群已入库的消息 ID。", nil
		}
		label := strings.TrimPrefix(name, "as_")
		if err := a.Store.Label(ctx, updateID, m.Chat.ID, target.ID, m.UserID(), label, target.Bytes()); err != nil {
			return "", err
		}
		diagnostic.Info("manual annotation recorded", "event", "annotation.recorded", "target_message", target.ID, "label", label)
		return fmt.Sprintf("消息 %d 已标注为 %s；模型判断和原始内容已保留。", target.ID, label), nil
	}
	return help, nil
}
