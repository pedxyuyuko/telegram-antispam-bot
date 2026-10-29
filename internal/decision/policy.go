package decision

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/expression"
)

type EffectiveAnswer struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Raw    Answer `json:"raw"`
}

type Term struct {
	Name      string           `json:"name"`
	Condition expression.Truth `json:"condition"`
	Status    string           `json:"status"`
	Add       float64          `json:"add"`
}

type Evaluation struct {
	Answers   map[string]EffectiveAnswer `json:"answers"`
	Values    map[string]float64         `json:"values"`
	Score     float64                    `json:"score"`
	Terms     []Term                     `json:"terms"`
	Condition expression.Truth           `json:"condition"`
}

// EvaluatePolicy is pure: it consumes the frozen policy and raw answers, records
// per-question abstentions, then computes independent terms and the final gate.
func EvaluatePolicy(questions map[string]config.Question, p config.Policy, result Result) (Evaluation, error) {
	e := Evaluation{Answers: map[string]EffectiveAnswer{}, Values: map[string]float64{}, Terms: []Term{}, Condition: expression.True}
	for id, q := range questions {
		a, present := result.Answers[id]
		// Older callers and archived workflows may only carry noul probabilities.
		if result.Answers == nil && q.Type == "noul" {
			if v, ok := result.Probabilities[id]; ok {
				a = Answer{Type: "noul", Noul: &v}
				present = true
			}
		}
		effective := EffectiveAnswer{Status: "unknown", Raw: a}
		switch {
		case !present || a.Type != q.Type:
			effective.Reason = "missing_answer"
		case q.Type == "noul":
			if a.Noul != nil && probability(*a.Noul) {
				effective.Status = "answered"
				e.Values["p."+id] = *a.Noul
			} else {
				effective.Reason = "missing_answer"
			}
		case a.Confidence == nil:
			effective.Reason = "missing_confidence"
		case !probability(*a.Confidence) || *a.Confidence < p.MinConfidence:
			effective.Reason = "low_confidence"
		default:
			effective.Status = "answered"
			if q.Type == "choice" {
				for option, v := range a.Probabilities {
					e.Values["p."+id+"."+option] = v
				}
			}
			if q.Type == "score" && a.Score != nil {
				e.Values["s."+id] = *a.Score
			}
		}
		e.Answers[id] = effective
	}
	variables := config.Variables(questions)
	if p.Scoring != nil {
		e.Score = p.Scoring.Initial
		for _, rule := range p.Scoring.Rules {
			program, err := expression.Compile(rule.When, variables, false)
			if err != nil {
				return e, err
			}
			term := Term{Name: rule.Name, Add: rule.Add, Condition: expression.Unknown, Status: "skipped_unknown"}
			if !program.Missing(e.Values) {
				term.Condition = program.Eval(e.Values, 0)
				term.Status = "unmatched"
				if term.Condition == expression.True {
					e.Score += rule.Add
					term.Status = "applied"
				}
			}
			e.Terms = append(e.Terms, term)
		}
	}
	if p.Condition != "" {
		program, err := expression.Compile(p.Condition, variables, true)
		if err != nil {
			return e, err
		}
		e.Condition = program.Eval(e.Values, e.Score)
	}
	return e, nil
}

// PolicyKey excludes punishment details so count/window/action edits retain
// applicable evidence. Legacy noul rules retain their original series hash.
func PolicyKey(p config.Policy, questions map[string]config.Question, r config.Rule) string {
	var raw []byte
	if p.Scoring == nil && p.Condition == "" && r.Question != "" {
		raw, _ = json.Marshal(struct {
			Question  config.Question
			Threshold float64
		}{questions[r.Question], r.Threshold})
	} else {
		raw, _ = json.Marshal(struct {
			Questions     map[string]config.Question
			Scoring       *config.Scoring
			Condition     string
			MinConfidence float64
			Question      string
			Threshold     float64
		}{questions, p.Scoring, p.Condition, p.MinConfidence, r.Question, r.Threshold})
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
