package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func joinConfigJSON() string {
	return `{"owner_ids":[9],"model":{"name":"m"},"question_groups":{"join":{"promotion":{"type":"noul","instructions":"member promotion"},"money":{"type":"noul","instructions":"member money"},"earnings":{"type":"noul","instructions":"member earnings"},"sexual":{"type":"noul","instructions":"member sexual"}}},"questions":{"v":{"type":"noul","instructions":"message"}},"base":{"mode":"enforce","rules":{}},"groups":{"-100":{"mode":"record"}},"join_gate":{"base":{"enabled":true},"groups":{"-100":{"enabled":false},"-200":{"mode":"enforce","queue_limit":2}}}}`
}

func TestJoinPolicyInheritsIndependentlyAndHonorsExplicitOff(t *testing.T) {
	c, err := Decode([]byte(joinConfigJSON()))
	if err != nil {
		t.Fatal(err)
	}
	if p := c.ForJoinGroup(-100); p.Enabled || p.Mode != "record" || p.Trigger != "member_joined" {
		t.Fatalf("explicit off lost: %+v", p)
	}
	if p := c.ForJoinGroup(-200); !p.Enabled || p.Mode != "enforce" || p.QueueLimit != 2 {
		t.Fatalf("join override lost: %+v", p)
	}
	if c.ForJoinGroup(-300).Mode != "record" || c.ForGroup(-300).Mode != "enforce" {
		t.Fatal("message and join modes leaked across policies")
	}
	questions, err := c.SelectedQuestions(c.ForJoinGroup(-300).Policy)
	if err != nil || len(questions) != 4 {
		t.Fatalf("join selection=%v %v", questions, err)
	}
	legacy, err := Decode([]byte(`{"owner_ids":[9],"model":{"name":"m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.ForJoinGroup(-100).Enabled {
		t.Fatal("upgrade enabled join punishment")
	}
}

func TestJoinCommandChangesPersistWithoutChangingMessagePolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(joinConfigJSON()), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot()
	chat := int64(-100)
	if err := m.SetJoin(&chat, "enabled", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	if err := m.SetJoin(&chat, "mode", json.RawMessage(`"enforce"`)); err != nil {
		t.Fatal(err)
	}
	m, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	after := m.Snapshot()
	if !after.ForJoinGroup(chat).Enabled || after.ForJoinGroup(chat).Mode != "enforce" {
		t.Fatal("join command not persisted")
	}
	if !reflect.DeepEqual(before.Base, after.Base) || !reflect.DeepEqual(before.Groups, after.Groups) {
		t.Fatal("join command changed message policy")
	}
	raw, _ := os.ReadFile(path)
	for _, tc := range []struct{ path, value string }{{"mode", `"oops"`}, {"trigger", `"join_request"`}, {"queue_limit", "0"}, {"enabled", "null"}, {"rules.remove.action", `"ban"`}, {"rules.remove.count", "2"}, {"rules.remove.target", `"caller"`}, {"rules.remove.question", `"join.money"`}, {"typo", "1"}} {
		if err := m.SetJoin(&chat, tc.path, json.RawMessage(tc.value)); err == nil {
			t.Fatalf("accepted %s=%s", tc.path, tc.value)
		}
	}
	unchanged, _ := os.ReadFile(path)
	if string(raw) != string(unchanged) {
		t.Fatal("invalid join command changed file")
	}
	if err := os.WriteFile(path, append(raw, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.SetJoin(&chat, "enabled", json.RawMessage(`false`)); err == nil {
		t.Fatal("overwrote concurrent config edit")
	}
}

func TestJoinConfigurationRejectsUnknownFieldsAndPreservesEmptyRules(t *testing.T) {
	for _, raw := range []string{
		strings.Replace(joinConfigJSON(), `"enabled":true`, `"enabled":true,"enabld":true`, 1),
		strings.Replace(joinConfigJSON(), `"-200"`, `"200"`, 1),
		strings.Replace(joinConfigJSON(), `"enabled":false`, `"enabled":null`, 1),
	} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Fatal("accepted malformed join configuration")
		}
	}
	raw := strings.Replace(joinConfigJSON(), `"base":{"enabled":true}`, `"base":{"enabled":true,"rules":{}}`, 1)
	c, err := Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ForJoinGroup(-200).Rules) != 0 {
		t.Fatal("empty rule map resurrected default removal")
	}
}
