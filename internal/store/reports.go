package store

import "telegram-antispam-bot/internal/decision"

// Reports describe committed outcomes for diagnostics. They never carry message
// bodies, profile text, credentials, or the raw model response.
type ArchiveReport struct {
	ID        int64
	Status    string
	Duplicate bool
	Evicted   int64
}

type QueueChange struct {
	ChatID  int64
	Limit   int
	Evicted int64
}

type RuleReport struct {
	ID            string   `json:"rule"`
	Status        string   `json:"status"`
	Question      string   `json:"question,omitempty"`
	Threshold     float64  `json:"threshold,omitempty"`
	Probability   *float64 `json:"probability,omitempty"`
	NewHit        bool     `json:"new_hit"`
	Hits          int      `json:"hits"`
	Required      int      `json:"required"`
	WindowSeconds int      `json:"window_seconds"`
}

type PlannedAction struct {
	ID     int64  `json:"id"`
	RuleID string `json:"rule"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Until  int64  `json:"until,omitempty"`
}

type FinishReport struct {
	AttemptID  int64
	Status     string
	Reason     string
	RetryAt    int64
	Evaluation *decision.Evaluation
	Rules      []RuleReport
	Actions    []PlannedAction
}
