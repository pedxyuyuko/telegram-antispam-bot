package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"telegram-antispam-bot/internal/config"
)

func saveScoringConfig(t *testing.T, a *App, path string, c config.Config) {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(filepath.Dir(path), "config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Config.Reload(); err != nil {
		t.Fatal(err)
	}
}

func groupedModel(t *testing.T, a *App, path, mode string) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]config.Question `json:"questions"`
			State     map[string]json.RawMessage `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if len(req.Questions) != 2 || req.Questions["message.fraud"].Instructions != "fraud?" {
			t.Error("request lost selected/frozen question definitions")
		}
		if !strings.Contains(string(req.State["sender_profile"]), "profile") {
			t.Error("profile material lost")
		}
		w.Write([]byte(`{"model":"m","answers":{"message.fraud":{"type":"noul","noul":0.99},"context.kind":{"type":"choice","choice":"ham","probabilities":{"spam":0.01,"ham":0.99},"confidence":0.1}}}`))
	}))
	t.Cleanup(s.Close)
	c := a.Config.Snapshot()
	c.Model.BaseURL = s.URL
	c.QuestionGroups = map[string]map[string]config.Question{
		"message": {"fraud": {Type: "noul", Instructions: "fraud?"}, "extra": {Type: "noul", Instructions: "unused?"}},
		"context": {"kind": {Type: "choice", Instructions: "kind?", Criteria: json.RawMessage(`{"spam":"promotion","ham":"chat"}`)}},
	}
	c.Base.Mode = mode
	c.Base.MinConfidence = .75
	c.Base.Questions = []string{"message.fraud", "context.kind"}
	c.Base.Scoring = &config.Scoring{Rules: []config.ScoreRule{{Name: "fraud", When: "p.message.fraud > 0.9", Add: 60}, {Name: "benign", When: "p.context.kind.ham > 0.9", Add: -50}}}
	c.Base.Condition = "score >= 60"
	c.Base.Rules = map[string]config.Rule{"delete": {Enabled: true, Count: 1, Action: "delete"}}
	saveScoringConfig(t, a, path, c)
}

func TestGroupedPolicyRunsFromInboxToAuditAndModeration(t *testing.T) {
	for _, mode := range []string{"record", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			var deletions atomic.Int32
			a, path := appFixture(t, "member", func(r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "deleteMessage") {
					deletions.Add(1)
				}
			})
			groupedModel(t, a, path, mode)
			deliver(t, a, 1, message(50, "promotion"))
			runModel(t, a, time.Now())
			if scalar(t, path, `SELECT count(*) FROM evaluations WHERE json_extract(result,'$.score')=60 AND json_extract(result,'$.condition')='true' AND json_extract(result,'$.answers."context.kind".status')='unknown' AND json_extract(result,'$.answers."message.fraud".status')='answered'`) != 1 {
				t.Fatal("score/abstention audit missing")
			}
			worked, err := a.ProcessAction(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := int32(0)
			if mode == "enforce" {
				want = 1
			}
			if deletions.Load() != want || worked != (mode == "enforce") {
				t.Fatalf("mode=%s deletions=%d worked=%v", mode, deletions.Load(), worked)
			}
		})
	}
}

func TestDecisionDependencyChangesCancelQueuedPunishments(t *testing.T) {
	for _, change := range []string{"confidence", "condition", "score", "selection", "question", "criteria"} {
		t.Run(change, func(t *testing.T) {
			a, path := appFixture(t, "member")
			groupedModel(t, a, path, "enforce")
			deliver(t, a, 1, message(50, "promotion"))
			runModel(t, a, time.Now())
			c := a.Config.Snapshot()
			switch change {
			case "confidence":
				c.Base.MinConfidence = .9
			case "condition":
				c.Base.Condition = "score > 100"
			case "score":
				c.Base.Scoring.Rules[0].Add = 50
			case "selection":
				c.Base.Questions = append(c.Base.Questions, "message.extra")
			case "question":
				q := c.QuestionGroups["message"]["fraud"]
				q.Instructions = "new question?"
				c.QuestionGroups["message"]["fraud"] = q
			case "criteria":
				q := c.QuestionGroups["context"]["kind"]
				q.Criteria = json.RawMessage(`{"spam":"new standard","ham":"chat"}`)
				c.QuestionGroups["context"]["kind"] = q
			}
			saveScoringConfig(t, a, path, c)
			worked, err := a.ProcessAction(context.Background())
			if err != nil || !worked {
				t.Fatalf("action: %v %v", worked, err)
			}
			if scalar(t, path, `SELECT count(*) FROM actions WHERE status='skipped'`) != 1 {
				t.Fatal("stale decision dispatched")
			}
		})
	}
}

func TestQuestionSelectionAndDefinitionsStayFrozenWhileQueued(t *testing.T) {
	a, path := appFixture(t, "member")
	groupedModel(t, a, path, "record")
	deliver(t, a, 1, message(50, "promotion"))
	c := a.Config.Snapshot()
	q := c.QuestionGroups["message"]["fraud"]
	q.Instructions = "changed after receipt?"
	c.QuestionGroups["message"]["fraud"] = q
	c.Base.Questions = append(c.Base.Questions, "message.extra")
	saveScoringConfig(t, a, path, c)
	runModel(t, a, time.Now())
	if scalar(t, path, `SELECT count(*) FROM evaluations`) != 1 {
		t.Fatal("queued inference lost its snapshot")
	}
}
