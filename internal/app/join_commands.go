package app

import (
	"context"
	"encoding/json"
	"strings"

	"telegram-antispam-bot/internal/telegram"
)

const joinUsage = "用法：/as_join on | off | status | mode record | mode enforce"

func (a *App) joinCommand(ctx context.Context, m *telegram.Message, args string) (string, error) {
	if !m.IsGroup() {
		return "请在目标群里管理入群检测。", nil
	}
	fields := strings.Fields(args)
	if len(fields) == 1 && fields[0] == "status" {
		p := a.Config.Snapshot().ForJoinGroup(m.Chat.ID)
		stats, err := a.Store.JoinStats(ctx, m.Chat.ID)
		if err != nil {
			return "", err
		}
		raw, _ := json.MarshalIndent(map[string]any{"effective": p, "arrivals": stats}, "", "  ")
		return "入群检测有效配置：\n" + string(raw), nil
	}
	var path string
	var value json.RawMessage
	if len(fields) == 1 && (fields[0] == "on" || fields[0] == "off") {
		path = "enabled"
		value = json.RawMessage(`false`)
		if fields[0] == "on" {
			value = json.RawMessage(`true`)
		}
	} else if len(fields) == 2 && fields[0] == "mode" && (fields[1] == "record" || fields[1] == "enforce") {
		path = "mode"
		value, _ = json.Marshal(fields[1])
	} else {
		return joinUsage, nil
	}
	if err := a.Config.SetJoin(&m.Chat.ID, path, value); err != nil {
		return "入群检测配置修改失败：" + err.Error(), nil
	}
	if err := a.Store.ResizeJoin(ctx, a.Config.Snapshot()); err != nil {
		return "", err
	}
	p := a.Config.Snapshot().ForJoinGroup(m.Chat.ID)
	a.Log.Info("join configuration updated", "event", "join.config_updated", "chat", m.Chat.ID, "actor", m.UserID(), "setting", path, "enabled", p.Enabled, "mode", p.Mode)
	if !p.Enabled {
		return "入群检测已关闭，配置已保存。", nil
	}
	if p.Mode == "record" {
		return "入群检测已开启，当前只记录判定；配置已保存。", nil
	}
	return "入群检测已开启，当前执行模式会移出命中成员；配置已保存。", nil
}
