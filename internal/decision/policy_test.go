package decision

import (
	"encoding/json"
	"testing"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/expression"
)

func number(v float64) *float64 { return &v }

func TestPolicyUsesNoulProbabilityRegardlessOfConfidence(t *testing.T) {
	q := map[string]config.Question{"message.fraud": {Type: "noul", Instructions: "fraud?"}}
	p := config.Policy{MinConfidence: .99, Condition: "p.message.fraud > 0.9"}
	for _, confidence := range []*float64{nil, number(0), number(1)} {
		e, err := EvaluatePolicy(q, p, Result{Answers: map[string]Answer{"message.fraud": {Type: "noul", Noul: number(.98), Confidence: confidence}}})
		if err != nil {
			t.Fatal(err)
		}
		if e.Condition != expression.True || e.Answers["message.fraud"].Status != "answered" {
			t.Fatalf("noul was confidence-gated: %+v", e)
		}
	}
}

func TestPolicySkipsWholeScoreTermForAbstainedQuestions(t *testing.T) {
	q := map[string]config.Question{
		"message.promotion": {Type: "noul", Instructions: "promotion?"},
		"context.kind":      {Type: "choice", Instructions: "kind?", Criteria: json.RawMessage(`{"ham":"chat","spam":"ad"}`)},
		"message.severity":  {Type: "score", Instructions: "level?", Criteria: json.RawMessage(`["none","high"]`)},
	}
	p := config.Policy{MinConfidence: .75, Scoring: &config.Scoring{Initial: 5, Rules: []config.ScoreRule{
		{Name: "promotion", When: "p.message.promotion >= 0.8", Add: 50},
		{Name: "normal", When: "p.context.kind.ham > 0.5", Add: -40},
		{Name: "mixed", When: "true || s.message.severity > 0.5", Add: 100},
	}}, Condition: "score >= 60 || s.message.severity > 0.8"}
	r := Result{Answers: map[string]Answer{
		"message.promotion": {Type: "noul", Noul: number(.9)},
		"context.kind":      {Type: "choice", Choice: "ham", Probabilities: map[string]float64{"ham": .9, "spam": .1}, Confidence: number(.74)},
		"message.severity":  {Type: "score", Score: number(.9)},
	}}
	e, err := EvaluatePolicy(q, p, r)
	if err != nil {
		t.Fatal(err)
	}
	if e.Score != 55 || e.Condition != expression.Unknown || e.Terms[1].Status != "skipped_unknown" || e.Terms[2].Status != "skipped_unknown" {
		t.Fatalf("unknown leaked into scoring: %+v", e)
	}
	if e.Answers["context.kind"].Reason != "low_confidence" || e.Answers["message.severity"].Reason != "missing_confidence" {
		t.Fatalf("missing abstention reasons: %+v", e.Answers)
	}
	if *e.Answers["context.kind"].Raw.Confidence != .74 {
		t.Fatal("raw confidence lost")
	}
	p.Scoring.Initial = 10
	e, err = EvaluatePolicy(q, p, r)
	if err != nil || e.Score != 60 || e.Condition != expression.True {
		t.Fatalf("known OR branch failed: %+v %v", e, err)
	}
	a := r.Answers["context.kind"]
	a.Confidence = number(.75)
	r.Answers["context.kind"] = a
	e, err = EvaluatePolicy(q, p, r)
	if err != nil || e.Score != 20 || e.Terms[1].Status != "applied" {
		t.Fatalf("threshold equality or subtraction wrong: %+v %v", e, err)
	}
}

func TestPolicyFingerprintSeparatesDecisionChangesFromPunishments(t *testing.T) {
	c := config.Defaults()
	r := c.Base.Rules["repeat"]
	key := PolicyKey(c.Base, c.Questions, r)
	r.Count = 9
	r.WindowSeconds = 900
	r.DurationSeconds = 120
	if PolicyKey(c.Base, c.Questions, r) != key {
		t.Fatal("punishment changes discarded evidence")
	}
	r.Threshold = .5
	if PolicyKey(c.Base, c.Questions, r) == key {
		t.Fatal("threshold change reused evidence")
	}
	r = c.Base.Rules["repeat"]
	c.Base.Condition = "score >= 60"
	c.Base.Scoring = &config.Scoring{Rules: []config.ScoreRule{{Name: "a", When: "p.fraud > 0.8", Add: 60}}}
	key = PolicyKey(c.Base, c.Questions, r)
	c.Base.Scoring.Rules[0].Add = 50
	if PolicyKey(c.Base, c.Questions, r) == key {
		t.Fatal("score changes reused evidence")
	}
}

func TestConfidentScoreUsesExpectedLevelAndCanProduceNegativeTotal(t *testing.T) {
	q := map[string]config.Question{"severity": {Type: "score", Instructions: "level?", Criteria: json.RawMessage(`["none","low","high"]`)}}
	p := config.Policy{MinConfidence: .75, Scoring: &config.Scoring{Initial: 10, Rules: []config.ScoreRule{{Name: "low risk", When: "s.severity < 0.5", Add: -50}}}, Condition: "score < 0"}
	e, err := EvaluatePolicy(q, p, Result{Answers: map[string]Answer{"severity": {Type: "score", Score: number(.2), Confidence: number(.75)}}})
	if err != nil {
		t.Fatal(err)
	}
	if e.Values["s.severity"] != .2 || e.Score != -40 || e.Condition != expression.True || e.Answers["severity"].Status != "answered" {
		t.Fatalf("score answer was lost or total clamped: %+v", e)
	}
}

func TestCollidingVariablesCannotMaskAnAbstainedAnswer(t *testing.T) {
	q := map[string]config.Question{
		"kind":      {Type: "choice", Instructions: "kind?", Criteria: json.RawMessage(`{"spam":"ad","ham":"chat"}`)},
		"kind.spam": {Type: "noul", Instructions: "spam?"},
	}
	p := config.Policy{MinConfidence: .75, Scoring: &config.Scoring{Rules: []config.ScoreRule{{Name: "ad", When: "p.kind.spam > 0.9", Add: 60}}}, Condition: "score >= 60"}
	for _, confidence := range []float64{.1, .99} {
		r := Result{Answers: map[string]Answer{"kind": {Type: "choice", Choice: "ham", Probabilities: map[string]float64{"ham": .99, "spam": .01}, Confidence: number(confidence)}, "kind.spam": {Type: "noul", Noul: number(.99)}}}
		if _, err := EvaluatePolicy(q, p, r); err == nil {
			t.Fatal("ambiguous archived policy evaluated")
		}
	}
}
