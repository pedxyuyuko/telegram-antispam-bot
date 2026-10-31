package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"telegram-antispam-bot/internal/store"
	"telegram-antispam-bot/internal/telegram"
)

func parseBlacklistEdit(text string) (store.BlacklistEdit, error) {
	var edit store.BlacklistEdit
	decoder := json.NewDecoder(strings.NewReader(text))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return edit, errors.New("编辑内容须为 JSON 对象")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return edit, err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return edit, errors.New("字段名须唯一")
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return edit, err
		}
		if string(value) == "null" {
			return edit, errors.New("使用空字符串清空备注；active 使用 true 或 false")
		}
		switch key {
		case "note":
			var note string
			if err := json.Unmarshal(value, &note); err != nil {
				return edit, errors.New("note 须为字符串")
			}
			if utf8.RuneCountInString(note) > 1000 {
				return edit, errors.New("备注最多 1000 个字符")
			}
			edit.Note = &note
		case "active":
			var active bool
			if err := json.Unmarshal(value, &active); err != nil {
				return edit, errors.New("active 须为布尔值")
			}
			edit.Active = &active
		default:
			return edit, errors.New("仅支持 note 和 active 字段")
		}
	}
	if _, err := decoder.Token(); err != nil {
		return edit, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return edit, errors.New("请提供单个 JSON 对象")
	}
	if len(seen) == 0 {
		return edit, errors.New("至少提供 note 或 active 字段")
	}
	return edit, nil
}

func blacklistDetails(entry store.BlacklistEntry) string {
	status := "停用"
	if entry.Active {
		status = "启用"
	}
	source := map[string]string{"manual": "所有者操作", "automatic": "广告删除后自动加入", "history": "历史删除记录导入"}[entry.Source]
	if source == "" {
		source = entry.Source
	}
	note := entry.Note
	if note == "" {
		note = "（空）"
	}
	actor := "系统"
	if entry.ActorID > 0 {
		actor = strconv.FormatInt(entry.ActorID, 10)
	}
	lines := []string{botLabel(entry.BotIdentity), "状态：" + status, "备注：" + note, "来源：" + source, "最近状态操作人：" + actor, fmt.Sprintf("名单代次：%d", entry.Generation), "创建时间：" + time.UnixMilli(entry.CreatedAt).UTC().Format("2006-01-02 15:04:05 UTC"), "更新时间：" + time.UnixMilli(entry.UpdatedAt).UTC().Format("2006-01-02 15:04:05 UTC")}
	if entry.SourceActionID > 0 {
		lines = append(lines, fmt.Sprintf("源删除动作：%d", entry.SourceActionID))
	}
	return strings.Join(lines, "\n")
}

func (a *App) editBotBlacklistCommand(ctx context.Context, update int64, m *telegram.Message, args string) (string, error) {
	text := strings.TrimSpace(args)
	ref := ""
	if !strings.HasPrefix(text, "{") {
		fields := strings.Fields(text)
		if len(fields) < 2 {
			return `用法：/as_botedit <bot ID 或 @用户名> {"note":"备注","active":true}；回复 bot 时可省略目标。`, nil
		}
		ref = fields[0]
		text = strings.TrimSpace(text[len(ref):])
	}
	edit, err := parseBlacklistEdit(text)
	if err != nil {
		return "编辑失败：" + err.Error(), nil
	}
	bot, err := a.resolveBlacklistTarget(ctx, m, ref, true)
	if reply, handled := blacklistTargetError(err); handled {
		return reply, nil
	}
	if err != nil {
		return "", err
	}
	if edit.Active != nil && *edit.Active {
		if bot.ID == a.Telegram.Me.ID || a.Config.Snapshot().IsOwner(bot.ID) {
			return "当前机器人和所有者身份保持保护。", nil
		}
		if _, err := a.Store.ResolveBot(ctx, strconv.FormatInt(bot.ID, 10)); err != nil {
			if reply, handled := blacklistTargetError(err); handled {
				return reply, nil
			}
			return "", err
		}
	}
	change, err := a.Store.EditBotBlacklist(ctx, bot.ID, edit, m.UserID(), update)
	if errors.Is(err, store.ErrBlacklistEntryNotFound) {
		return "该 bot 尚无名单记录，请先使用 /as_botban 添加。", nil
	}
	if err != nil {
		return "", err
	}
	if change.Duplicate {
		return "这条黑名单命令已经处理。", nil
	}
	a.diagnostics(m.Chat.ID, "message", m.ID, "update", update).Info("global bot blacklist edited", "event", "blacklist.updated", "bot_id", bot.ID, "actor", m.UserID(), "changed", change.Changed, "note_supplied", edit.Note != nil, "active_supplied", edit.Active != nil)
	entry, err := a.Store.BotBlacklist(ctx, bot.ID)
	if err != nil {
		return "", err
	}
	return "已更新全局 bot 黑名单记录。\n" + blacklistDetails(*entry), nil
}
