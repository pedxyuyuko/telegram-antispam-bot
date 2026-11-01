package config

import (
	"encoding/json"
	"testing"
)

func TestCallerBanConfigurationValidatesTargetAndDuration(t *testing.T) {
	for _, tc := range []struct {
		name, rule string
		valid      bool
	}{
		{"permanent", `{"enabled":true,"target":"caller","action":"ban","count":1,"duration_seconds":-1}`, true},
		{"finite", `{"enabled":true,"target":"caller","action":"ban","count":1,"duration_seconds":60}`, true},
		{"maximum", `{"enabled":true,"target":"caller","action":"ban","count":1,"duration_seconds":31536000}`, true},
		{"too_short", `{"target":"caller","action":"ban","count":1,"duration_seconds":29}`, false},
		{"zero", `{"target":"caller","action":"ban","count":1,"duration_seconds":0}`, false},
		{"too_long", `{"target":"caller","action":"ban","count":1,"duration_seconds":31622401}`, false},
		{"negative", `{"target":"caller","action":"ban","count":1,"duration_seconds":-2}`, false},
		{"sender_ban", `{"action":"ban","count":1,"question":"violation","duration_seconds":-1}`, false},
		{"caller_mute", `{"target":"caller","action":"mute","count":1,"duration_seconds":60}`, false},
		{"unknown_target", `{"target":"unknown","action":"delete","count":1,"question":"violation"}`, false},
		{"caller_count", `{"target":"caller","action":"ban","count":2,"window_seconds":60,"duration_seconds":-1}`, false},
		{"caller_question", `{"target":"caller","action":"ban","count":1,"question":"violation","duration_seconds":-1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode([]byte(`{"owner_ids":[9],"model":{"name":"test"},"base":{"rules":{"ban":` + tc.rule + `}}}`))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestCallerBanDefaultsStayDisabledAndInheritPerGroup(t *testing.T) {
	m, err := Open(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	r := m.Snapshot().Base.Rules["ban"]
	if r.Action != "ban" || r.Enabled || r.DurationSeconds != -1 {
		t.Fatalf("unsafe or missing ban default: %+v", r)
	}
	chat := int64(-100)
	if err := m.Set(&chat, "rules.ban.duration_seconds", json.RawMessage(`3600`)); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(&chat, "rules.ban.enabled", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	c := m.Snapshot()
	if !c.ForGroup(chat).Rules["ban"].Enabled || c.ForGroup(chat).Rules["ban"].DurationSeconds != 3600 || c.ForGroup(-200).Rules["ban"].Enabled {
		t.Fatal("group ban settings leaked")
	}
}
