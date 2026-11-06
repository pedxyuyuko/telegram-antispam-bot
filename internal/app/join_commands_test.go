package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"telegram-antispam-bot/internal/config"
)

func configureJoin(t *testing.T, a *App, dbpath, mode string, enabled bool) {
	t.Helper()
	c := a.Config.Snapshot()
	if c.QuestionGroups == nil {
		c.QuestionGroups = map[string]map[string]config.Question{}
	}
	c.QuestionGroups["join"] = map[string]config.Question{}
	for _, id := range []string{"promotion", "money", "earnings", "sexual"} {
		c.QuestionGroups["join"][id] = config.Question{Type: "noul", Instructions: "Evaluate .member.name and .member.bio: " + id}
	}
	p := config.DefaultJoinPolicy()
	p.Enabled = enabled
	p.Mode = mode
	c.JoinGate = &config.JoinGate{Base: p, Groups: map[string]json.RawMessage{}}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(dbpath), "config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Config.Reload(); err != nil {
		t.Fatal(err)
	}
}

func TestJoinCommandsPersistSeparateGroupSwitchAndMode(t *testing.T) {
	a, path := appFixture(t, "administrator")
	configureJoin(t, a, path, "record", false)
	before := a.Config.Snapshot()
	deliver(t, a, 1, message(42, "/as_join on"))
	if !a.Config.Snapshot().ForJoinGroup(-100).Enabled || a.Config.Snapshot().ForJoinGroup(-200).Enabled {
		t.Fatal("join switch did not stay in this group")
	}
	deliver(t, a, 2, message(42, "/as_join mode enforce"))
	deliver(t, a, 3, message(42, "/as_join off"))
	p := a.Config.Snapshot().ForJoinGroup(-100)
	if p.Enabled || p.Mode != "enforce" {
		t.Fatal("off discarded previous mode")
	}
	deliver(t, a, 4, message(42, "/as_join@guard_bot on"))
	m, err := config.Open(filepath.Join(filepath.Dir(path), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if p = m.Snapshot().ForJoinGroup(-100); !p.Enabled || p.Mode != "enforce" {
		t.Fatal("join commands did not survive restart")
	}
	after := a.Config.Snapshot()
	if !reflect.DeepEqual(before.Base, after.Base) || !reflect.DeepEqual(before.Groups, after.Groups) {
		t.Fatal("join commands changed message configuration")
	}
	for i, text := range []string{"/as_join status", "/as_join mode invalid", "/as_join on extra"} {
		deliver(t, a, int64(i+5), message(42, text))
	}
	if !a.Config.Snapshot().ForJoinGroup(-100).Enabled || a.Config.Snapshot().ForJoinGroup(-100).Mode != "enforce" {
		t.Fatal("status/invalid syntax changed state")
	}
}

func TestJoinCommandsRequireCurrentGroupAdministrator(t *testing.T) {
	for _, role := range []string{"member", "", "administrator"} {
		t.Run(role, func(t *testing.T) {
			a, path := appFixture(t, role)
			configureJoin(t, a, path, "record", false)
			m := message(9, "/as_join on")
			if role == "administrator" {
				m.Chat.ID = 9
				m.Chat.Type = "private"
			}
			deliver(t, a, 1, m)
			if a.Config.Snapshot().ForJoinGroup(-100).Enabled || a.Config.Snapshot().JoinGate.Base.Enabled {
				t.Fatal("owner/private command bypassed group authority")
			}
		})
	}
}
