package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	data := `{"owner_ids":[9],"model":{"name":"test-model"},"groups":{"-100":{"mode":"enforce","debug":false,"rules":{"repeat":{"duration_seconds":120}}}}}`
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOverridesInheritIndividualFieldsAndExplicitFalse(t *testing.T) {
	m, err := Open(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Set(nil, "debug", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	c := m.Snapshot()
	p := c.ForGroup(-100)
	if p.Mode != "enforce" || p.Debug || p.Rules["repeat"].DurationSeconds != 120 || p.Rules["repeat"].Count != 3 {
		t.Fatalf("unexpected merged policy: %+v", p)
	}
	if c.ForGroup(-200).Mode != "record" || !c.ForGroup(-200).Debug {
		t.Fatal("shared policy was not inherited")
	}
	if err := m.Set(nil, "rules.repeat.count", json.RawMessage(`4`)); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().ForGroup(-100).Rules["repeat"].Count != 4 {
		t.Fatal("group stopped inheriting shared changes")
	}
}

func TestCommandChangesSurviveRestartAndResetRestoresInheritance(t *testing.T) {
	p := fixture(t)
	m, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	chat := int64(-100)
	if err := m.Set(&chat, "rules.repeat.enabled", json.RawMessage(`false`)); err != nil {
		t.Fatal(err)
	}
	m, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().ForGroup(chat).Rules["repeat"].Enabled {
		t.Fatal("explicit false was lost")
	}
	if err := m.Reset(chat, "rules.repeat.enabled"); err != nil {
		t.Fatal(err)
	}
	if !m.Snapshot().ForGroup(chat).Rules["repeat"].Enabled {
		t.Fatal("reset failed to restore inheritance")
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe config mode: %v", info.Mode())
	}
}

func TestInvalidUpdatesLeaveMemoryAndFileUntouched(t *testing.T) {
	p := fixture(t)
	m, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)
	for _, tc := range []struct{ key, value string }{
		{"queue_limit", "0"}, {"rules.repeat.threshold", "1.1"}, {"rules.repeat.question", `"missing"`},
		{"rules.repeat.duration_seconds", "10"}, {"rules.repeat.count", "0"}, {"typo", "1"},
		{"mode", "null"}, {"rules.repeat.threshold", "null"}, {"rules.repeat.count", "2.5"},
	} {
		if err := m.Set(nil, tc.key, json.RawMessage(tc.value)); err == nil {
			t.Errorf("accepted %s=%s", tc.key, tc.value)
		}
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) || m.Snapshot().Base.QueueLimit != 100 {
		t.Fatal("invalid changes mutated live configuration")
	}
}

func TestExternalEditsRequireReloadBeforeCommandWrite(t *testing.T) {
	p := fixture(t)
	m, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	raw = []byte(strings.Replace(string(raw), `"name":"test-model"`, `"name":"new-model"`, 1))
	if err := os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(nil, "debug", json.RawMessage(`true`)); err == nil {
		t.Fatal("overwrote a concurrent file edit")
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Model.Name != "new-model" {
		t.Fatal("reload had no effect")
	}
	if err := m.Set(nil, "debug", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"oops":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err == nil {
		t.Fatal("invalid reload accepted")
	}
	if m.Snapshot().Model.Name != "new-model" {
		t.Fatal("invalid reload replaced active config")
	}
}

func TestRejectsAmbiguousAndUnknownConfig(t *testing.T) {
	for _, raw := range []string{
		`{"owner_ids":[9],"model":{"name":"m"},"wat":1}`,
		`{"owner_ids":[9],"model":{"name":"m"},"groups":{"-100":{"debig":true}}}`,
		`{"owner_ids":[9],"model":{"name":"m"},"groups":{"-100":{"debug":null}}}`,
		`{"owner_ids":[9],"model":{"name":"m"}} {}`,
		`{"owner_ids":[9],"model":{"name":"m"},"base":{"queue_limit":1,"queue_limit":2}}`,
	} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Errorf("accepted invalid config: %s", raw)
		}
	}
}
