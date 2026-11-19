package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestExplicitSharedRuleMapCanBeClearedAcrossReloadAndRestart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"owner_ids":[9],"model":{"name":"m"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Set(nil, "rules", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if len(m.Snapshot().Base.Rules) != 0 {
		t.Fatal("clear reported success but retained old rules")
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if len(m.Snapshot().Base.Rules) != 0 {
		t.Fatal("reload resurrected default punishments")
	}
	m, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Snapshot().Base.Rules) != 0 {
		t.Fatal("restart resurrected default punishments")
	}
}

func TestExplicitQuestionSetReplacesDefaults(t *testing.T) {
	c, err := Decode([]byte(`{"owner_ids":[9],"model":{"name":"m"},"base":{"rules":{}},"questions":{"custom":{"type":"noul","instructions":"custom question"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Questions) != 1 || len(c.Base.Rules) != 0 {
		t.Fatal("explicit dictionaries inherited unwanted defaults")
	}
}

func TestImageInferenceTimeoutHasIndependentValidatedDefault(t *testing.T) {
	c, err := Decode([]byte(`{"owner_ids":[9],"model":{"name":"m","image_timeout_seconds":90}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Model.ImageTimeoutSeconds != 90 || c.Model.TimeoutSeconds != 20 {
		t.Fatal("image timeout replaced text timeout")
	}
	for _, value := range []string{"0", "3601"} {
		if _, err := Decode([]byte(`{"owner_ids":[9],"model":{"name":"m","image_timeout_seconds":` + value + `}}`)); err == nil {
			t.Fatal("invalid image timeout accepted")
		}
	}
}

func TestPublicPagesDefaultOffAndReloadWithoutRestart(t *testing.T) {
	p := fixture(t)
	m, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if pages := m.Snapshot().PublicPages; pages.Enabled || pages.Cache() != 24*time.Hour {
		t.Fatalf("t.me page defaults=%+v", pages)
	}
	raw, _ := os.ReadFile(p)
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	object["public_pages"] = json.RawMessage(`{"enabled":true,"cache_seconds":3600}`)
	raw, _ = json.Marshal(object)
	if err := os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatalf("public page switch needs a restart: %v", err)
	}
	if pages := m.Snapshot().PublicPages; !pages.Enabled || pages.Cache() != time.Hour {
		t.Fatalf("reloaded=%+v", pages)
	}
	for _, value := range []string{`true`, `{"enabled":"yes"}`, `{"enabled":true,"cache_seconds":-1}`, `{"enabled":true,"cache_seconds":604801}`, `{"enabled":true,"ttl":5}`} {
		if _, err := Decode([]byte(`{"owner_ids":[9],"model":{"name":"m"},"public_pages":` + value + `}`)); err == nil {
			t.Errorf("accepted public_pages=%s", value)
		}
	}
	c, err := Decode([]byte(`{"owner_ids":[9],"model":{"name":"m"},"public_pages":{"enabled":true,"cache_seconds":0}}`))
	if err != nil || c.PublicPages.Cache() != 0 {
		t.Fatalf("zero must disable the separate page cache: %+v %v", c.PublicPages, err)
	}
}
