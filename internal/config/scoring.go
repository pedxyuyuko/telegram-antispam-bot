package config

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"telegram-antispam-bot/internal/expression"
)

type Scoring struct {
	Initial float64     `json:"initial"`
	Rules   []ScoreRule `json:"rules,omitempty"`
}

type ScoreRule struct {
	Name string  `json:"name"`
	When string  `json:"when"`
	Add  float64 `json:"add"`
}

func (q Question) Validate() error {
	if strings.TrimSpace(q.Instructions) == "" {
		return fmt.Errorf("instructions must be nonempty")
	}
	switch q.Type {
	case "noul":
		if len(q.Criteria) == 0 {
			return nil
		}
		var criteria map[string]string
		if err := strict(q.Criteria, &criteria); err != nil {
			return err
		}
		if len(criteria) != 2 || strings.TrimSpace(criteria["true"]) == "" || strings.TrimSpace(criteria["false"]) == "" {
			return fmt.Errorf("noul criteria require true and false descriptions")
		}
	case "choice":
		var criteria map[string]string
		if err := strict(q.Criteria, &criteria); err != nil {
			return err
		}
		if len(criteria) < 2 || len(criteria) > 255 {
			return fmt.Errorf("choice requires 2..255 options")
		}
		for name, text := range criteria {
			if !identifier.MatchString(name) || strings.TrimSpace(text) == "" {
				return fmt.Errorf("choice options require identifier names and nonempty descriptions")
			}
		}
	case "score":
		var criteria []string
		if err := strict(q.Criteria, &criteria); err != nil {
			return err
		}
		if len(criteria) < 2 || len(criteria) > 10 {
			return fmt.Errorf("score requires 2..10 ordered levels")
		}
		for _, text := range criteria {
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf("score descriptions must be nonempty")
			}
		}
	default:
		return fmt.Errorf("question type must be noul, choice or score")
	}
	return nil
}

// SelectedQuestions freezes full definitions, retaining legacy flat questions
// when selection is omitted. Group wildcards expand once at message receipt.
func (c Config) SelectedQuestions(p Policy) (map[string]Question, error) {
	out := map[string]Question{}
	if p.Questions == nil {
		for id, q := range c.Questions {
			out[id] = q
		}
	} else {
		add := func(id string, q Question) error {
			if _, exists := out[id]; exists {
				return fmt.Errorf("duplicate selected question %q", id)
			}
			out[id] = q
			return nil
		}
		for _, ref := range p.Questions {
			if group, wildcard := strings.CutSuffix(ref, ".*"); wildcard {
				qs, ok := c.QuestionGroups[group]
				if !ok {
					return nil, fmt.Errorf("unknown question group %q", group)
				}
				for id, q := range qs {
					if err := add(group+"."+id, q); err != nil {
						return nil, err
					}
				}
				continue
			}
			q, exists := c.Questions[ref]
			if group, id, grouped := strings.Cut(ref, "."); grouped {
				q, exists = c.QuestionGroups[group][id]
			}
			if !exists {
				return nil, fmt.Errorf("unknown question %q", ref)
			}
			if err := add(ref, q); err != nil {
				return nil, err
			}
		}
	}
	if len(out) == 0 || len(out) > 256 {
		return nil, fmt.Errorf("select between 1 and 256 questions")
	}
	return out, nil
}

// Variables describes numeric expression references. Score questions expose the
// model's expected level as s.<question>; score is the bot's separate total.
func Variables(questions map[string]Question) ([]string, error) {
	var out []string
	for id, q := range questions {
		switch q.Type {
		case "noul":
			out = append(out, "p."+id)
		case "choice":
			var criteria map[string]string
			_ = json.Unmarshal(q.Criteria, &criteria)
			for option := range criteria {
				out = append(out, "p."+id+"."+option)
			}
		case "score":
			out = append(out, "s."+id)
		}
	}
	sort.Strings(out)
	for i := 1; i < len(out); i++ {
		if out[i] == out[i-1] {
			return nil, fmt.Errorf("selected questions produce conflicting variable %q", out[i])
		}
	}
	return out, nil
}

func (p Policy) validateScoring(questions map[string]Question) error {
	if math.IsNaN(p.MinConfidence) || math.IsInf(p.MinConfidence, 0) || p.MinConfidence < 0 || p.MinConfidence > 1 {
		return fmt.Errorf("min_confidence must be in [0,1]")
	}
	if p.OnLowConfidence != "" && p.OnLowConfidence != "abstain_question" {
		return fmt.Errorf("on_low_confidence must be abstain_question")
	}
	variables, err := Variables(questions)
	if err != nil {
		return err
	}
	if p.Condition != "" {
		if _, err := expression.Compile(p.Condition, variables, true); err != nil {
			return fmt.Errorf("condition: %w", err)
		}
	}
	if p.Scoring == nil {
		return nil
	}
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) <= 1e9 }
	if !finite(p.Scoring.Initial) || len(p.Scoring.Rules) > 128 {
		return fmt.Errorf("scoring initial must be finite within +/-1e9, with at most 128 terms")
	}
	names := map[string]bool{}
	for _, r := range p.Scoring.Rules {
		if strings.TrimSpace(r.Name) == "" || names[r.Name] || !finite(r.Add) {
			return fmt.Errorf("score terms require unique nonempty names and finite additions within +/-1e9")
		}
		names[r.Name] = true
		if _, err := expression.Compile(r.When, variables, false); err != nil {
			return fmt.Errorf("scoring %s: %w", r.Name, err)
		}
	}
	return nil
}
