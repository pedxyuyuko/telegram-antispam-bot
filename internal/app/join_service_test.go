package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"telegram-antispam-bot/internal/telegram"
)

func joinServiceMessage(id int, date int64, users ...int64) *telegram.Message {
	m := message(99, "")
	m.ID = id
	m.Date = date
	for _, id := range users {
		m.NewChatMembers = append(m.NewChatMembers, telegram.User{ID: id, FirstName: "Joined"})
	}
	return m
}

func runJoinCleanup(t *testing.T, a *App) {
	t.Helper()
	if _, err := a.ProcessJoinServiceAction(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestJoinCleanupDeletesOnlyMatchedViolatingHintInEitherOrder(t *testing.T) {
	for _, late := range []bool{false, true} {
		a, path, r := joinApp(t, "enforce", true)
		date := time.Now().Unix() - 1
		if !late {
			deliver(t, a, 1, joinServiceMessage(701, date, 50))
		}
		deliverMembership(t, a, 2, -100, 50, "left", "member", date)
		runJoin(t, a, time.Now())
		runJoinAction(t, a)
		r.role.Store("left")
		if late {
			deliver(t, a, 3, joinServiceMessage(701, date, 50))
		}
		runJoinCleanup(t, a)
		runJoinCleanup(t, a)
		if r.deletes.Load() != 1 || r.deletedID.Load() != 701 || r.kicks.Load() != 1 {
			t.Fatalf("late=%v delete=%d message=%d kick=%d", late, r.deletes.Load(), r.deletedID.Load(), r.kicks.Load())
		}
		if scalar(t, path, `SELECT count(*) FROM revisions WHERE message_id=701 AND status='ignored' AND json_extract(raw,'$.new_chat_members[0].id')=50 AND json_type(snapshot,'$.join')='object'`) != 1 {
			t.Fatal("service original or receipt policy lost")
		}
		if scalar(t, path, `SELECT count(*) FROM join_service_actions WHERE status='succeeded' AND json_array_length(evidence)=1`) != 1 {
			t.Fatal("cleanup evidence/outcome lost")
		}
	}
}

func TestJoinCleanupPreservesNormalMixedUnknownAndUnmatchedHints(t *testing.T) {
	for _, which := range []string{"normal", "mixed", "unknown", "date", "record", "disabled", "delete_rule_disabled"} {
		t.Run(which, func(t *testing.T) {
			mode := "enforce"
			if which == "record" {
				mode = "record"
			}
			a, path, r := joinApp(t, mode, which != "disabled")
			date := time.Now().Unix() - 1
			chat := int64(-100)
			if which == "delete_rule_disabled" {
				if err := a.Config.SetJoin(&chat, "rules.delete_join_message.enabled", json.RawMessage(`false`)); err != nil {
					t.Fatal(err)
				}
			}
			users := []int64{50}
			if which == "mixed" || which == "unknown" {
				users = append(users, 51)
			}
			hintDate := date
			if which == "date" {
				hintDate--
			}
			deliver(t, a, 1, joinServiceMessage(701, hintDate, users...))
			if which == "normal" {
				r.benign.Store(true)
			}
			deliverMembership(t, a, 2, -100, 50, "left", "member", date)
			if which != "disabled" {
				runJoin(t, a, time.Now())
			}
			if which == "mixed" {
				r.benign.Store(true)
				deliverMembership(t, a, 3, -100, 51, "left", "member", date)
				runJoin(t, a, time.Now())
			}
			runJoinCleanup(t, a)
			if r.deletes.Load() != 0 {
				t.Fatalf("deleted %s hint", which)
			}
			if which == "record" && scalar(t, path, `SELECT count(*) FROM join_service_actions WHERE status='recorded'`) != 1 {
				t.Fatal("record-mode cleanup not audited")
			}
		})
	}
}

func TestJoinCleanupSharedHintRequiresAllMembersToViolate(t *testing.T) {
	a, path, r := joinApp(t, "enforce", true)
	date := time.Now().Unix() - 1
	deliver(t, a, 1, joinServiceMessage(701, date, 50, 51))
	deliverMembership(t, a, 2, -100, 50, "left", "member", date)
	runJoin(t, a, time.Now())
	runJoinCleanup(t, a)
	if r.deletes.Load() != 0 {
		t.Fatal("unknown second member lost its hint")
	}
	deliverMembership(t, a, 3, -100, 51, "left", "member", date)
	runJoin(t, a, time.Now())
	runJoinCleanup(t, a)
	if r.deletes.Load() != 1 || scalar(t, path, `SELECT count(*) FROM join_service_actions WHERE status='succeeded' AND json_array_length(evidence)=2`) != 1 {
		t.Fatal("all-member evidence was not retained")
	}
}

func TestJoinCleanupRechecksPermissionsPolicyAndAge(t *testing.T) {
	for _, change := range []string{"administrator", "creator", "unavailable", "off_on", "rule", "condition", "expired"} {
		t.Run(change, func(t *testing.T) {
			a, path, r := joinApp(t, "enforce", true)
			date := time.Now().Unix() - 1
			if change == "expired" {
				date -= 49 * 3600
			}
			deliver(t, a, 1, joinServiceMessage(701, date, 50))
			deliverMembership(t, a, 2, -100, 50, "left", "member", date)
			runJoin(t, a, time.Now())
			var armed atomic.Bool
			armed.Store(true)
			r.beforeMembership = func() {
				if !armed.Swap(false) {
					return
				}
				chat := int64(-100)
				switch change {
				case "off_on":
					for _, cmd := range []string{"off", "on"} {
						if _, err := a.joinCommand(context.Background(), message(42, ""), cmd); err != nil {
							t.Error(err)
						}
					}
				case "rule":
					if err := a.Config.SetJoin(&chat, "rules.delete_join_message.enabled", json.RawMessage(`false`)); err != nil {
						t.Error(err)
					}
				case "condition":
					if err := a.Config.SetJoin(&chat, "condition", json.RawMessage(`"score >= 200"`)); err != nil {
						t.Error(err)
					}
				}
			}
			switch change {
			case "administrator", "creator", "unavailable":
				r.role.Store(change)
			}
			runJoinCleanup(t, a)
			if r.deletes.Load() != 0 || scalar(t, path, `SELECT count(*) FROM join_service_actions WHERE status='skipped'`) != 1 {
				t.Fatalf("unsafe cleanup after %s", change)
			}
		})
	}
}

func TestJoinCleanupRecordsFailureAndDoesNotReplay(t *testing.T) {
	a, path, r := joinApp(t, "enforce", true)
	r.deleteFail.Store(true)
	date := time.Now().Unix() - 1
	deliver(t, a, 1, joinServiceMessage(701, date, 50))
	deliverMembership(t, a, 2, -100, 50, "left", "member", date)
	runJoin(t, a, time.Now())
	runJoinCleanup(t, a)
	runJoinCleanup(t, a)
	if r.deletes.Load() != 1 || scalar(t, path, `SELECT count(*) FROM join_service_actions WHERE status='failed' AND detail<>''`) != 1 {
		t.Fatal("cleanup failure lost or repeated")
	}
}

func TestJoinCleanupRuntimeDrainsDeletionAlongsideMemberRemoval(t *testing.T) {
	a, path, r := joinApp(t, "enforce", true)
	date := time.Now().Unix() - 1
	service, _ := json.Marshal(map[string]any{"update_id": 1, "message": joinServiceMessage(701, date, 50)})
	r.pollBatch = []json.RawMessage{service, memberUpdate(2, -100, 50, "left", "member", date)}
	cfg := a.Config.Snapshot().Telegram
	cfg.APIBase = a.Config.Snapshot().Model.BaseURL
	tg, err := telegram.New(cfg, "123:offline", &telegram.DurableClient{HTTP: &http.Client{Timeout: 2 * time.Second}, Capture: a.Store.Capture}, 0, a.Log)
	if err != nil {
		t.Fatal(err)
	}
	tg.Me = a.Telegram.Me
	a.Telegram = tg
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	finished := false
	for !finished {
		select {
		case <-ctx.Done():
			finished = true
		case <-ticker.C:
			var n int
			if err := db.QueryRow(`SELECT (SELECT count(*) FROM join_service_actions WHERE status='succeeded')+(SELECT count(*) FROM join_actions WHERE status='succeeded')`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			finished = n == 2
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r.models.Load() != 1 || r.kicks.Load() != 1 || r.deletes.Load() != 1 {
		t.Fatalf("model=%d kick=%d delete=%d", r.models.Load(), r.kicks.Load(), r.deletes.Load())
	}
}
