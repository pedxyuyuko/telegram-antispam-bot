package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/telegram"
)

func TestArchiveReportsOnlyActualEvictionsAndDuplicates(t *testing.T) {
	s := openTest(t)
	p := config.Defaults().Base
	p.QueueLimit = 1
	first := add(t, s, 1, -100, 1, 42, p)
	m := &telegram.Message{ID: 2, Chat: telegram.Chat{ID: -100, Type: "supergroup"}, From: &telegram.User{ID: 42}, Text: "private text"}
	raw, _ := json.Marshal(map[string]any{"update_id": 2, "message": m})
	if err := s.Capture(context.Background(), []json.RawMessage{raw}); err != nil {
		t.Fatal(err)
	}
	in := Inbox{ID: 2, Raw: raw, ReceivedAt: testNow.UnixMilli()}
	r, err := s.AddRevision(context.Background(), in, m, Snapshot{Policy: p, Questions: config.Defaults().Questions}, "pending")
	if err != nil || r.ID <= first || r.Evicted != 1 || r.Status != "pending" || r.Duplicate {
		t.Fatalf("archive report: %+v %v", r, err)
	}
	old, _ := s.Revision(context.Background(), first)
	if old.Status != "evicted" {
		t.Fatal("report announced an eviction that did not persist")
	}
	duplicate, err := s.AddRevision(context.Background(), in, m, Snapshot{Policy: p}, "pending")
	if err != nil || duplicate.ID != r.ID || !duplicate.Duplicate || duplicate.Evicted != 0 {
		t.Fatalf("duplicate report: %+v %v", duplicate, err)
	}
}

func TestFinishReportsScoringCountAndRecordedActions(t *testing.T) {
	s := openTest(t)
	p := config.Defaults().Base
	add(t, s, 1, -100, 1, 42, p)
	j, _ := s.Claim(context.Background(), testNow)
	r, err := s.Finish(context.Background(), *j, decision.Result{Probabilities: map[string]float64{"violation": .99}}, nil, testNow, 10)
	if err != nil {
		t.Fatal(err)
	}
	if r.AttemptID == 0 || r.Status != "succeeded" || r.Evaluation == nil || len(r.Rules) != 2 || len(r.Actions) != 1 {
		t.Fatalf("incomplete result: %+v", r)
	}
	if r.Rules[0].Status != "eligible" || r.Rules[0].Hits != 1 || !r.Rules[0].NewHit || r.Rules[1].Status != "below_count" || r.Rules[1].Required != 3 {
		t.Fatalf("incorrect rule counters: %+v", r.Rules)
	}
	if r.Actions[0].Status != "recorded" || r.Actions[0].Kind != "delete" || r.Actions[0].ID == 0 {
		t.Fatalf("incorrect record-mode action: %+v", r.Actions)
	}
	add(t, s, 2, -100, 1, 42, p)
	j, _ = s.Claim(context.Background(), testNow)
	r, err = s.Finish(context.Background(), *j, decision.Result{Probabilities: map[string]float64{"violation": .99}}, nil, testNow, 10)
	if err != nil || r.Rules[0].NewHit || r.Rules[0].Hits != 1 {
		t.Fatalf("edit reported another hit: %+v %v", r, err)
	}
}

func TestFinishReportsRetryAndStaleOutcomes(t *testing.T) {
	for _, scenario := range []string{"retry", "evicted_failure", "superseded"} {
		t.Run(scenario, func(t *testing.T) {
			s := openTest(t)
			p := config.Defaults().Base
			p.QueueLimit = 1
			add(t, s, 1, -100, 1, 42, p)
			j, _ := s.Claim(context.Background(), testNow)
			var inferenceErr error
			if scenario != "superseded" {
				inferenceErr = errors.New("offline")
			}
			if scenario == "evicted_failure" {
				add(t, s, 2, -100, 2, 42, p)
			}
			if scenario == "superseded" {
				p.QueueLimit = 2
				add(t, s, 2, -100, 1, 42, p)
			}
			r, err := s.Finish(context.Background(), *j, decision.Result{Probabilities: map[string]float64{"violation": 1}}, inferenceErr, testNow, 10)
			if err != nil {
				t.Fatal(err)
			}
			want := "pending"
			if scenario == "evicted_failure" {
				want = "evicted"
			}
			if scenario == "superseded" {
				want = "superseded"
			}
			if r.Status != want || r.AttemptID == 0 || len(r.Actions) != 0 {
				t.Fatalf("incorrect final status: %+v", r)
			}
			if inferenceErr != nil && r.RetryAt != testNow.Add(10*time.Second).UnixMilli() {
				t.Fatal("retry deadline lost")
			}
		})
	}
}

func TestResizeReportsCommittedQueueChanges(t *testing.T) {
	s := openTest(t)
	c := config.Defaults()
	p := c.Base
	add(t, s, 1, -100, 1, 42, p)
	add(t, s, 2, -100, 2, 42, p)
	c.Base.QueueLimit = 1
	report, err := s.Resize(context.Background(), c)
	if err != nil || len(report) != 1 || report[0].ChatID != -100 || report[0].Limit != 1 || report[0].Evicted != 1 {
		t.Fatalf("resize report: %+v %v", report, err)
	}
}
