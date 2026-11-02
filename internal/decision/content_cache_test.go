package decision

import (
	"encoding/json"
	"testing"

	"telegram-antispam-bot/internal/config"
)

func contentPolicy() (map[string]config.Question, config.Policy) {
	questions := map[string]config.Question{"content.ad": {Type: "noul", Instructions: "message ad?"}, "profile.ad": {Type: "noul", Instructions: "profile ad?"}}
	p := config.Policy{Condition: "score >= 100", Scoring: &config.Scoring{Rules: []config.ScoreRule{{Name: "message", When: "p.content.ad >= 0.83", Add: 100}, {Name: "profile", When: "p.profile.ad >= 0.85", Add: 100}}}}
	return questions, p
}
func TestContentProjectionRequiresIndependentMessageEvidence(t *testing.T) {
	for _, tc := range []struct {
		name             string
		content, profile float64
		condition        string
		profileAdd       float64
		want             bool
	}{
		{"both", .9, .9, "score >= 100", 100, true}, {"content_only", .9, .1, "score >= 100", 100, true},
		{"profile_only", .1, .9, "score >= 100", 100, false},
		{"profile_can_veto", .9, .1, "score >= 100", -100, false},
		{"upper_bound", .9, .1, "score < 150", 100, false},
		{"interior_hole", .9, .1, "score != 150", 50, false},
		{"requires_profile", .9, .9, "score >= 100 && p.profile.ad > 0.8", 100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, p := contentPolicy()
			p.Condition = tc.condition
			p.Scoring.Rules[1].Add = tc.profileAdd
			r, ok, err := ContentProjection(q, p, Result{Probabilities: map[string]float64{"content.ad": tc.content, "profile.ad": tc.profile}})
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.want {
				t.Fatalf("cacheable=%v want=%v", ok, tc.want)
			}
			if ok {
				if _, exists := r.Answers["profile.ad"]; exists {
					t.Fatal("foreign profile answer reused")
				}
				if _, exists := r.Answers["content.ad"]; !exists {
					t.Fatal("content answer lost")
				}
			}
		})
	}
}
func TestContentPolicyKeyIsolatesJudgmentChanges(t *testing.T) {
	q, p := contentPolicy()
	key := ContentPolicyKey(q, p)
	p.Mode = "record"
	p.Debug = true
	p.QueueLimit = 99
	if ContentPolicyKey(q, p) != key {
		t.Fatal("operational mode fragmented cache")
	}
	p.Condition = "score >= 200"
	if ContentPolicyKey(q, p) == key {
		t.Fatal("threshold reused old key")
	}
	q, p = contentPolicy()
	r := q["content.ad"]
	r.Criteria = json.RawMessage(`{"true":"ad","false":"ordinary"}`)
	q["content.ad"] = r
	if ContentPolicyKey(q, p) == key {
		t.Fatal("question edit reused old key")
	}
}

func TestContentProjectionPreservesFloatingPointScoringOrder(t *testing.T) {
	q, p := contentPolicy()
	p.Condition = "score >= 0"
	p.Scoring.Rules = []config.ScoreRule{
		{Name: "first", When: "p.content.ad > 0.8", Add: 1e9},
		{Name: "unknown_penalty", When: "p.profile.ad > 0.8", Add: -1e9},
		{Name: "last", When: "p.content.ad > 0.8", Add: -1e-8},
	}
	_, ok, err := ContentProjection(q, p, Result{Probabilities: map[string]float64{"content.ad": .99, "profile.ad": .01}})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("reordered interval arithmetic hid a failing profile-dependent score")
	}
}
