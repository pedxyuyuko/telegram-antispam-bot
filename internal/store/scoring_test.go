package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/expression"
)

func scoringSnapshot() Snapshot {
	p := config.Defaults().Base
	p.Mode = "enforce"
	p.MinConfidence = .75
	p.Scoring = &config.Scoring{Rules: []config.ScoreRule{
		{Name: "promotion", When: "p.fraud >= 0.9", Add: 60},
		{Name: "normal", When: "p.kind.ham >= 0.9", Add: -50},
	}}
	p.Condition = "score >= 60 || p.kind.spam >= 0.98"
	p.Rules = map[string]config.Rule{"delete": {Enabled: true, Count: 1, Action: "delete"}}
	return Snapshot{Policy: p, Questions: map[string]config.Question{
		"fraud": {Type: "noul", Instructions: "fraud?"},
		"kind":  {Type: "choice", Instructions: "kind?", Criteria: json.RawMessage(`{"spam":"ad","ham":"chat"}`)},
	}}
}

func addScoring(t *testing.T, s *Store, update int64, message int, snapshot Snapshot) int64 {
	t.Helper()
	id := add(t, s, update, -100, message, 42, snapshot.Policy)
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE revisions SET snapshot=? WHERE id=?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func scoredResult(p float64, confidence *float64) decision.Result {
	return decision.Result{Request: []byte(`{"model":"m"}`), Response: []byte(`{"raw":"preserved"}`), Probabilities: map[string]float64{"fraud": p}, Answers: map[string]decision.Answer{
		"fraud": {Type: "noul", Noul: &p},
		"kind":  {Type: "choice", Choice: "ham", Probabilities: map[string]float64{"ham": .99, "spam": .01}, Confidence: confidence},
	}}
}

func TestScoringRecordsUnknownConditionsWithoutRetryingOrPunishing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		p          float64
		confidence *float64
		score      float64
		condition  expression.Truth
		action     bool
	}{
		{"known_or", .99, nil, 60, expression.True, true},
		{"unknown_or", .5, nil, 0, expression.Unknown, false},
		{"deduct", .99, ptr(.9), 10, expression.False, false},
		{"low_confidence", .99, ptr(.1), 60, expression.True, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t)
			snap := scoringSnapshot()
			id := addScoring(t, s, 1, 1, snap)
			j, err := s.Claim(context.Background(), testNow)
			if err != nil || j == nil {
				t.Fatalf("claim: %v %v", j, err)
			}
			if _, err := s.Finish(context.Background(), *j, scoredResult(tc.p, tc.confidence), nil, testNow, 10); err != nil {
				t.Fatal(err)
			}
			var raw string
			if err := s.db.QueryRow(`SELECT result FROM evaluations WHERE attempt_id=(SELECT id FROM attempts WHERE revision_id=?)`, id).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var e decision.Evaluation
			if err := json.Unmarshal([]byte(raw), &e); err != nil {
				t.Fatal(err)
			}
			if e.Score != tc.score || e.Condition != tc.condition {
				t.Fatalf("wrong evidence: %+v", e)
			}
			stored, _ := s.Revision(context.Background(), id)
			if stored.Status != "succeeded" {
				t.Fatalf("abstention retried: %s", stored.Status)
			}
			a, err := s.TakeAction(context.Background())
			if err != nil || (a != nil) != tc.action {
				t.Fatalf("action: %+v %v", a, err)
			}
		})
	}
}

func ptr(v float64) *float64 { return &v }

func TestScoringArchivesStaleAndEvictedEvaluationsWithoutHits(t *testing.T) {
	for _, evict := range []bool{false, true} {
		s := openTest(t)
		snap := scoringSnapshot()
		if evict {
			snap.Policy.QueueLimit = 1
		}
		addScoring(t, s, 1, 1, snap)
		j, _ := s.Claim(context.Background(), testNow)
		addScoring(t, s, 2, 1, snap)
		if _, err := s.Finish(context.Background(), *j, scoredResult(.99, nil), nil, testNow, 10); err != nil {
			t.Fatal(err)
		}
		var evaluations, hits int
		if err := s.db.QueryRow(`SELECT count(*) FROM evaluations`).Scan(&evaluations); err != nil {
			t.Fatal(err)
		}
		s.db.QueryRow(`SELECT count(*) FROM hits`).Scan(&hits)
		if evaluations != 1 || hits != 0 {
			t.Fatalf("evict=%v evaluations=%d hits=%d", evict, evaluations, hits)
		}
	}
}

func TestScoringCountDeduplicatesEditsAndSeparatesChangedFormula(t *testing.T) {
	s := openTest(t)
	snap := scoringSnapshot()
	snap.Policy.Rules = map[string]config.Rule{"repeat": {Enabled: true, Count: 2, WindowSeconds: 600, Action: "mute", DurationSeconds: 60}}
	for i := int64(1); i <= 2; i++ {
		addScoring(t, s, i, 1, snap)
		j, _ := s.Claim(context.Background(), testNow)
		if _, err := s.Finish(context.Background(), *j, scoredResult(.99, nil), nil, testNow, 10); err != nil {
			t.Fatal(err)
		}
	}
	if a, _ := s.TakeAction(context.Background()); a != nil {
		t.Fatal("edited message counted twice")
	}
	snap.Policy.Scoring.Rules[0].Add = 70
	addScoring(t, s, 3, 2, snap)
	j, _ := s.Claim(context.Background(), testNow)
	if _, err := s.Finish(context.Background(), *j, scoredResult(.99, nil), nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.TakeAction(context.Background()); a != nil {
		t.Fatal("old formula's evidence reused")
	}
	addScoring(t, s, 4, 3, snap)
	j, _ = s.Claim(context.Background(), testNow)
	if _, err := s.Finish(context.Background(), *j, scoredResult(.99, nil), nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.TakeAction(context.Background()); a == nil || a.Kind != "mute" {
		t.Fatal("new formula never accumulated")
	}
}

func TestVersionOneDatabaseGainsEvaluationArchiveWithoutLosingEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	add(t, s, 1, -100, 1, 42, config.Defaults().Base)
	finish(t, s, .1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.Exec(`DROP TABLE IF EXISTS evaluations; PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version, count int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	s.db.QueryRow(`SELECT count(*) FROM attempts`).Scan(&count)
	if version != 2 || count != 1 {
		t.Fatalf("upgrade lost evidence: version=%d count=%d", version, count)
	}
	if _, err = s.db.Exec(`SELECT result FROM evaluations`); err != nil {
		t.Fatal(err)
	}
}
