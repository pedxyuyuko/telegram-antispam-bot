package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/telegram"
)

var testNow = time.Unix(2000000000, 0)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "bot.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func add(t *testing.T, s *Store, update int64, chat int64, message int, user int64, p config.Policy) int64 {
	t.Helper()
	m := &telegram.Message{ID: message, Chat: telegram.Chat{ID: chat, Type: "supergroup"}, From: &telegram.User{ID: user}, Date: testNow.Unix(), Text: "sample"}
	raw, _ := json.Marshal(map[string]any{"update_id": update, "message": m})
	if err := s.Capture(context.Background(), []json.RawMessage{raw}); err != nil {
		t.Fatal(err)
	}
	in := Inbox{ID: update, Raw: raw, ReceivedAt: testNow.UnixMilli()}
	id, err := s.AddRevision(context.Background(), in, m, Snapshot{Policy: p, Questions: config.Defaults().Questions}, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkInbox(context.Background(), update, ""); err != nil {
		t.Fatal(err)
	}
	return id
}
func finish(t *testing.T, s *Store, p float64) *Job {
	t.Helper()
	j, err := s.Claim(context.Background(), testNow)
	if err != nil || j == nil {
		t.Fatalf("claim: %+v %v", j, err)
	}
	err = s.Finish(context.Background(), *j, decision.Result{Request: []byte(`{"state":"sample"}`), Response: []byte(`{"kept":true}`), Probabilities: map[string]float64{"violation": p}}, nil, testNow, 10)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestQueueKeepsNewestNAndArchivesEveryMessage(t *testing.T) {
	s := openTest(t)
	p := config.Defaults().Base
	p.QueueLimit = 3
	var ids []int64
	for i := int64(1); i <= 4; i++ {
		ids = append(ids, add(t, s, i, -100, int(i), 42, p))
	}
	first, _ := s.Revision(context.Background(), ids[0])
	if first.Status != "evicted" {
		t.Fatalf("oldest survived: %s", first.Status)
	}
	stats, err := s.Stats(context.Background(), -100)
	if err != nil {
		t.Fatal(err)
	}
	if stats["pending"] != 3 || stats["evicted"] != 1 {
		t.Fatalf("messages were lost or queue unbounded: %v", stats)
	}
	for _, want := range ids[1:] {
		j := finish(t, s, 0)
		if j.ID != want {
			t.Fatalf("wrong retained order: got %d want %d", j.ID, want)
		}
	}
	if last, err := s.LastUpdateID(context.Background()); err != nil || last != 4 {
		t.Fatalf("checkpoint %d: %v", last, err)
	}
}

func TestFailureRequeuesBoundedlyAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p := config.Defaults().Base
	p.QueueLimit = 2
	first := add(t, s, 1, -100, 1, 42, p)
	j, _ := s.Claim(context.Background(), testNow)
	if err := s.Finish(context.Background(), *j, decision.Result{Request: []byte("request"), Response: []byte("503")}, errors.New("offline"), testNow, 10); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.Claim(context.Background(), testNow.Add(9*time.Second)); j != nil {
		t.Fatal("retry backoff ignored")
	}
	add(t, s, 2, -100, 2, 42, p)
	add(t, s, 3, -100, 3, 42, p)
	j, _ = s.Revision(context.Background(), first)
	if j.Status != "evicted" {
		t.Fatal("failed oldest was pinned forever")
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j, err = s.Claim(context.Background(), testNow.Add(10*time.Second))
	if err != nil || j == nil || j.MessageID != 2 {
		t.Fatalf("recovery: %+v %v", j, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM attempts WHERE error='offline' AND response='503'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("failed model attempt was discarded")
	}
}

func TestEditSupersedesInflightWorkAndCannotDoubleCount(t *testing.T) {
	s := openTest(t)
	p := config.Defaults().Base
	p.Mode = "enforce"
	old := add(t, s, 1, -100, 7, 42, p)
	j, _ := s.Claim(context.Background(), testNow)
	newID := add(t, s, 2, -100, 7, 42, p)
	if err := s.Finish(context.Background(), *j, decision.Result{Probabilities: map[string]float64{"violation": 1}}, nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
	oldJob, _ := s.Revision(context.Background(), old)
	if oldJob.Status != "superseded" {
		t.Fatal("stale result was activated")
	}
	if a, _ := s.TakeAction(context.Background()); a != nil {
		t.Fatal("stale result punished a newer version")
	}
	if finish(t, s, 1).ID != newID {
		t.Fatal("edited message did not get evaluated")
	}
	add(t, s, 3, -100, 7, 42, p)
	finish(t, s, 1)
	var count int
	s.db.QueryRow(`SELECT count(*) FROM hits WHERE chat_id=-100 AND message_id=7 AND rule_id='repeat'`).Scan(&count)
	if count != 1 {
		t.Fatalf("edits counted %d times", count)
	}
	var mutes int
	s.db.QueryRow(`SELECT count(*) FROM actions WHERE kind='mute'`).Scan(&mutes)
	if mutes != 0 {
		t.Fatal("one edited message reached repeat threshold")
	}
}

func TestCountsArePerChatPerSenderAndRespectWindowAndMode(t *testing.T) {
	s := openTest(t)
	p := config.Defaults().Base
	p.Mode = "enforce"
	for i := int64(1); i <= 3; i++ {
		add(t, s, i, -100, int(i), 42, p)
		finish(t, s, .9)
	}
	a, err := s.TakeAction(context.Background())
	if err != nil || a == nil || a.Kind != "mute" || a.UserID != 42 {
		t.Fatalf("repeat penalty missing: %+v %v", a, err)
	}
	add(t, s, 4, -100, 4, 99, p)
	finish(t, s, .9)
	add(t, s, 5, -200, 5, 42, p)
	finish(t, s, .9)
	if a, _ := s.TakeAction(context.Background()); a != nil {
		t.Fatal("counts leaked between senders or groups")
	}
	// A record-mode hit stays observational even after enforcement is enabled.
	p.Mode = "record"
	add(t, s, 6, -300, 6, 42, p)
	finish(t, s, 1)
	if a, _ := s.TakeAction(context.Background()); a != nil {
		t.Fatal("record mode created a live action")
	}
	p.Mode = "enforce"
	add(t, s, 7, -300, 7, 42, p)
	finish(t, s, .9)
	if a, _ := s.TakeAction(context.Background()); a != nil {
		t.Fatal("record-mode history caused a punishment")
	}
	// Old events outside the window cannot contribute to a fresh mute.
	s.db.Exec(`UPDATE hits SET occurred_at=? WHERE chat_id=-100`, testNow.Add(-11*time.Minute).UnixMilli())
	add(t, s, 8, -100, 8, 42, p)
	finish(t, s, .9)
	if a, _ := s.TakeAction(context.Background()); a != nil {
		t.Fatal("expired hits contributed to punishment")
	}
}

func TestCapturingDuplicatesAndAnnotationsPreservesEvidence(t *testing.T) {
	s := openTest(t)
	p := config.Defaults().Base
	id := add(t, s, 1, -100, 1, 42, p)
	finish(t, s, 0)
	if got := add(t, s, 1, -100, 1, 42, p); got != id {
		t.Fatal("duplicate update created a revision")
	}
	if err := s.Label(context.Background(), 2, -100, 1, 9, "spam", []byte(`{"text":"original"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Label(context.Background(), 2, -100, 1, 9, "spam", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	var attempts, labels int
	s.db.QueryRow(`SELECT count(*) FROM attempts`).Scan(&attempts)
	s.db.QueryRow(`SELECT count(*) FROM annotations`).Scan(&labels)
	if attempts != 1 || labels != 1 {
		t.Fatalf("evidence altered or annotation duplicated: %d %d", attempts, labels)
	}
}

func TestSupersededUnsentActionDoesNotSuppressNewestDecision(t *testing.T) {
	s := openTest(t)
	p := config.Defaults().Base
	p.Mode = "enforce"
	add(t, s, 1, -100, 1, 42, p)
	finish(t, s, 1)
	latest := add(t, s, 2, -100, 1, 42, p)
	finish(t, s, 1)
	a, err := s.TakeAction(context.Background())
	if err != nil || a == nil {
		t.Fatalf("old action: %+v %v", a, err)
	}
	if current, _ := s.ActionCurrent(context.Background(), *a); current {
		t.Fatal("old action still current")
	}
	if err := s.FinishAction(context.Background(), a.ID, "skipped", "superseded"); err != nil {
		t.Fatal(err)
	}
	a, err = s.TakeAction(context.Background())
	if err != nil || a == nil || a.RevisionID != latest {
		t.Fatalf("newest decision was suppressed: %+v %v", a, err)
	}
	if err := s.FinishAction(context.Background(), a.ID, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	add(t, s, 3, -100, 1, 42, p)
	finish(t, s, 1)
	if a, _ := s.TakeAction(context.Background()); a != nil {
		t.Fatal("successful punishment was replayed for an edit")
	}
}

func TestDatabaseHasOneOwningProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(path)
	if err == nil {
		other.Close()
		s.Close()
		t.Fatal("second instance can reset live queue and action state")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	other, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
}

func TestEveryQueuedEditIsJudgedButOnlyNewestCanCount(t *testing.T) {
	s := openTest(t)
	p := config.Defaults().Base
	p.Mode = "enforce"
	first := add(t, s, 1, -100, 1, 42, p)
	second := add(t, s, 2, -100, 1, 42, p)
	if got := finish(t, s, 1).ID; got != first {
		t.Fatalf("older queued version skipped: got %d want %d", got, first)
	}
	if a, _ := s.TakeAction(context.Background()); a != nil {
		t.Fatal("old version was punished")
	}
	if got := finish(t, s, 0).ID; got != second {
		t.Fatal("edited version skipped")
	}
	var attempts, hits int
	s.db.QueryRow(`SELECT count(*) FROM attempts`).Scan(&attempts)
	s.db.QueryRow(`SELECT count(*) FROM hits`).Scan(&hits)
	if attempts != 2 || hits != 0 {
		t.Fatalf("attempts=%d hits=%d", attempts, hits)
	}
}

func TestEditDuringActionPreflightLeavesNewestPunishmentEligible(t *testing.T) {
	for _, oldOutcome := range []string{"skipped", "succeeded"} {
		t.Run(oldOutcome, func(t *testing.T) {
			s := openTest(t)
			p := config.Defaults().Base
			p.Mode = "enforce"
			add(t, s, 1, -100, 1, 42, p)
			finish(t, s, 1)
			old, err := s.TakeAction(context.Background())
			if err != nil || old == nil {
				t.Fatalf("claim old: %+v %v", old, err)
			}
			latest := add(t, s, 2, -100, 1, 42, p)
			finish(t, s, 1)
			// An in-flight preflight keeps the next intent waiting for its outcome.
			if next, _ := s.TakeAction(context.Background()); next != nil {
				t.Fatal("two actions for one message executing concurrently")
			}
			if err := s.FinishAction(context.Background(), old.ID, oldOutcome, "old result"); err != nil {
				t.Fatal(err)
			}
			next, err := s.TakeAction(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if oldOutcome == "skipped" && (next == nil || next.RevisionID != latest) {
				t.Fatal("both old and new punishments were skipped")
			}
			if oldOutcome == "succeeded" && next != nil {
				t.Fatal("successful punishment was repeated")
			}
		})
	}
}
