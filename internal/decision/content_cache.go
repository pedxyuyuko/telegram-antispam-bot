package decision

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/expression"
)

func ContentPolicyKey(questions map[string]config.Question, p config.Policy) string {
	raw, _ := json.Marshal(struct {
		Version       int
		Questions     map[string]config.Question
		Scoring       *config.Scoring
		Condition     string
		Confidence    float64
		LowConfidence string
	}{1, questions, p.Scoring, p.Condition, p.MinConfidence, p.OnLowConfidence})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

// ContentProjection exports only content.* answers. It proves that changing
// every omitted answer still leaves the final condition true, including score
// penalties and upper bounds. Unproven cases stay on the normal model path.
func ContentProjection(questions map[string]config.Question, p config.Policy, result Result) (Result, bool, error) {
	projected := Result{Answers: map[string]Answer{}, Probabilities: map[string]float64{}}
	full, err := EvaluatePolicy(questions, p, result)
	if err != nil {
		return projected, false, err
	}
	if full.Condition != expression.True || p.Condition == "" {
		return projected, false, nil
	}
	for id, a := range full.Answers {
		if strings.HasPrefix(id, "content.") && a.Status == "answered" {
			projected.Answers[id] = a.Raw
			if a.Raw.Type == "noul" {
				projected.Probabilities[id] = *a.Raw.Noul
			}
		}
	}
	if len(projected.Answers) == 0 {
		return projected, false, nil
	}
	e, err := EvaluatePolicy(questions, p, projected)
	if err != nil {
		return projected, false, err
	}
	if e.Condition != expression.True {
		return projected, false, nil
	}
	variables, err := config.Variables(questions)
	if err != nil {
		return projected, false, err
	}
	low, high := e.Score, e.Score
	if p.Scoring != nil {
		for _, rule := range p.Scoring.Rules {
			program, err := expression.Compile(rule.When, variables, false)
			if err != nil {
				return projected, false, err
			}
			if program.Missing(e.Values) {
				if rule.Add < 0 {
					low += rule.Add
				} else {
					high += rule.Add
				}
			}
		}
	}
	condition, err := expression.Compile(p.Condition, variables, true)
	if err != nil {
		return projected, false, err
	}
	return projected, condition.EvalScoreRange(e.Values, low, high) == expression.True, nil
}
