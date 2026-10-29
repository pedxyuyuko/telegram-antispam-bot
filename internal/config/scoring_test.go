package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const groupedConfig = `{
 "owner_ids":[9],"model":{"name":"m"},"questions":{},
 "question_groups":{
  "profile":{"promotion":{"type":"noul","instructions":"资料有推广？","criteria":{"true":"推广招揽","false":"普通介绍"}}},
  "message":{"fraud":{"type":"noul","instructions":"诈骗？"},"kind":{"type":"choice","instructions":"用途？","criteria":{"spam":"推广","ham":"讨论"}},"severity":{"type":"score","instructions":"严重程度？","criteria":["无","低","高"]}}
 },
 "base":{"min_confidence":0.75,"on_low_confidence":"abstain_question","questions":["profile.*","message.*"],
  "scoring":{"initial":0,"rules":[{"name":"推广","when":"p.profile.promotion > 0.8","add":20},{"name":"正常","when":"p.message.kind.ham > 0.9","add":-50},{"name":"严重","when":"s.message.severity >= 1.5","add":50}]},
  "condition":"score >= 60 || p.message.fraud >= 0.98",
  "rules":{"delete":{"enabled":true,"count":1,"action":"delete"}}
 },
 "groups":{"-100":{"min_confidence":0.85},"-200":{"questions":["message.fraud"],"scoring":{"rules":[]},"condition":"p.message.fraud > 0.9"}}
}`

func TestGroupedQuestionsScoringAndLocalOverrides(t *testing.T) {
	c, err := Decode([]byte(groupedConfig))
	if err != nil {
		t.Fatal(err)
	}
	p := c.ForGroup(-100)
	qs, err := c.SelectedQuestions(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 4 || qs["profile.promotion"].Type != "noul" || p.MinConfidence != .85 || p.Scoring.Rules[1].Add != -50 {
		t.Fatalf("wrong selection or inheritance: %+v %+v", p, qs)
	}
	qs, err = c.SelectedQuestions(c.ForGroup(-200))
	if err != nil || len(qs) != 1 || qs["message.fraud"].Type != "noul" {
		t.Fatalf("selection was merged instead of replaced: %v %v", qs, err)
	}
	if c.ForGroup(-300).MinConfidence != .75 {
		t.Fatal("shared confidence lost")
	}
}

func TestRejectsUnknownReferencesAndInvalidQuestionCriteria(t *testing.T) {
	for _, change := range [][2]string{
		{`"profile.*"`, `"missing.*"`},
		{`"profile.*","message.*"`, `"profile.*","profile.promotion","message.*"`},
		{`p.profile.promotion > 0.8`, `p.profile.missing > 0.8`},
		{`p.profile.promotion > 0.8`, `score > 0.8`},
		{`score >= 60 || p.message.fraud >= 0.98`, `exec("rm")`},
		{`"min_confidence":0.75`, `"min_confidence":1.1`},
		{`"abstain_question"`, `"accept"`},
		{`"false":"普通介绍"`, `"flase":"普通介绍"`},
		{`"spam":"推广","ham":"讨论"`, `"spam":"推广"`},
		{`"无","低","高"`, `"无"`},
		{`"questions":["message.fraud"]`, `"questions":[]`},
		{`"add":20`, `"add":1e308`},
	} {
		raw := strings.Replace(groupedConfig, change[0], change[1], 1)
		if _, err := Decode([]byte(raw)); err == nil {
			t.Errorf("accepted %s", change[1])
		}
	}
}

func TestScoringEditsPersistAndResetToSharedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(groupedConfig), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	chat := int64(-100)
	if err := m.Set(&chat, "condition", json.RawMessage(`"score > 80"`)); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(&chat, "min_confidence", json.RawMessage(`0`)); err != nil {
		t.Fatal(err)
	}
	m, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().ForGroup(chat).Condition != "score > 80" || m.Snapshot().ForGroup(chat).MinConfidence != 0 {
		t.Fatal("explicit settings lost on restart")
	}
	if err := m.Reset(chat, "condition"); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().ForGroup(chat).Condition != m.Snapshot().Base.Condition {
		t.Fatal("condition inheritance lost")
	}
	if err := m.Set(&chat, "condition", json.RawMessage(`"p.unselected > 0"`)); err == nil {
		t.Fatal("accepted unknown question through command")
	}
}

func TestRejectsCollidingAnswerVariablesAcrossFlatAndGroupedQuestions(t *testing.T) {
	const raw = `{"owner_ids":[9],"model":{"name":"m"},"questions":{"kind":{"type":"choice","instructions":"kind?","criteria":{"spam":"ad","ham":"chat"}}},"question_groups":{"kind":{"spam":{"type":"noul","instructions":"spam?"}}},"base":{"questions":["kind"],"rules":{}}}`
	if _, err := Decode([]byte(raw)); err != nil {
		t.Fatalf("individually selected question failed: %v", err)
	}
	for _, selection := range []string{`["kind","kind.spam"]`, `["kind","kind.*"]`} {
		if _, err := Decode([]byte(strings.Replace(raw, `["kind"]`, selection, 1))); err == nil {
			t.Fatalf("accepted colliding selection %s", selection)
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	chat := int64(-100)
	if err := m.Set(&chat, "questions", json.RawMessage(`["kind","kind.spam"]`)); err == nil {
		t.Fatal("command activated ambiguous variables")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != raw {
		t.Fatal("invalid selection changed disk configuration")
	}
}
