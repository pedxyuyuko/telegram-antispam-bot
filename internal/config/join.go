package config

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// JoinPolicy is a separate policy namespace for one member's arrival. It shares
// question/scoring semantics with messages, while restricting actions to removal.
type JoinPolicy struct {
	Policy
	Enabled bool   `json:"enabled"`
	Trigger string `json:"trigger"`
}

type JoinGate struct {
	Base   JoinPolicy                 `json:"base"`
	Groups map[string]json.RawMessage `json:"groups"`
}

func DefaultJoinPolicy() JoinPolicy {
	return JoinPolicy{Trigger: "member_joined", Policy: Policy{
		Mode: "record", QueueLimit: 100, Questions: []string{"join.*"}, MinConfidence: .75, OnLowConfidence: "abstain_question",
		Scoring: &Scoring{Rules: []ScoreRule{{Name: "入群资料宣传金钱或色情", When: "p.join.promotion >= 0.85 && (p.join.money >= 0.85 || p.join.sexual >= 0.85)", Add: 100}}}, Condition: "score >= 100",
		Rules: map[string]Rule{"remove": {Enabled: true, Count: 1, Action: "kick"}},
	}}
}

func (c Config) mergeJoinPolicy(patch json.RawMessage) (JoinPolicy, error) {
	b, _ := json.Marshal(c.JoinGate.Base)
	merged, err := merge(b, patch)
	if err != nil {
		return JoinPolicy{}, err
	}
	var p JoinPolicy
	err = strict(merged, &p)
	return p, err
}

func (c Config) ForJoinGroup(chat int64) JoinPolicy {
	if c.JoinGate == nil {
		return DefaultJoinPolicy()
	}
	if patch, ok := c.JoinGate.Groups[strconv.FormatInt(chat, 10)]; ok {
		p, _ := c.mergeJoinPolicy(patch)
		return p
	}
	return c.JoinGate.Base
}

func (c Config) validateJoinPolicy(p JoinPolicy) error {
	if p.Trigger != "member_joined" {
		return fmt.Errorf("trigger must be member_joined")
	}
	if p.Scoring == nil || p.Condition == "" {
		return fmt.Errorf("join gate requires scoring and condition")
	}
	if err := c.validatePolicy(p.Policy); err != nil {
		return err
	}
	for id, r := range p.Rules {
		if r.Action != "kick" || r.Target != "" || r.Count != 1 || r.WindowSeconds != 0 || r.Question != "" || r.Threshold != 0 || r.DurationSeconds != 0 {
			return fmt.Errorf("rule %s: join gate requires kick, count 1 and the final condition", id)
		}
	}
	return nil
}

func (c Config) validateJoinGate() error {
	if c.JoinGate == nil {
		return nil
	}
	if err := c.validateJoinPolicy(c.JoinGate.Base); err != nil {
		return fmt.Errorf("join_gate.base: %w", err)
	}
	for id, patch := range c.JoinGate.Groups {
		chat, err := strconv.ParseInt(id, 10, 64)
		if err != nil || chat >= 0 || strconv.FormatInt(chat, 10) != id {
			return fmt.Errorf("invalid join group ID %q", id)
		}
		p, err := c.mergeJoinPolicy(patch)
		if err == nil {
			err = c.validateJoinPolicy(p)
		}
		if err != nil {
			return fmt.Errorf("join_gate.groups.%s: %w", id, err)
		}
	}
	return nil
}

func (m *Manager) SetJoin(chat *int64, path string, value json.RawMessage) error {
	return m.changePolicy(chat, path, value, false, true)
}
