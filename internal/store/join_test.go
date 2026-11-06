package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/telegram"
)

func joinSnapshot(mode string) JoinSnapshot {
	p := config.DefaultJoinPolicy()
	p.Enabled = true
	p.Mode = mode
	q := map[string]config.Question{}
	for _, id := range []string{"promotion", "money", "earnings", "sexual"} {
		q["join."+id] = config.Question{Type: "noul", Instructions: id}
	}
	return JoinSnapshot{Policy: p, Questions: q, ModelID: "m", ModelEndpoint: "http://127.0.0.1:8000"}
}

func captureMember(t *testing.T, s *Store, update, chat, user int64, old, next string, at ...int64) (Inbox, telegram.MembershipUpdate) {
	t.Helper()
	m := telegram.MembershipUpdate{Chat: telegram.Chat{ID: chat, Type: "supergroup"}, From: telegram.User{ID: 99}, Date: testNow.Unix() + update, Old: telegram.MemberState{User: telegram.User{ID: user}, Status: old}, New: telegram.MemberState{User: telegram.User{ID: user, FirstName: "member"}, Status: next}}
	if len(at) > 0 {
		m.Date = at[0]
	}
	raw, _ := json.Marshal(map[string]any{"update_id": update, "chat_member": m, "extra": "retained"})
	if err := s.Capture(context.Background(), []json.RawMessage{raw}); err != nil {
		t.Fatal(err)
	}
	return Inbox{ID: update, Raw: raw, ReceivedAt: testNow.UnixMilli()}, m
}

func addMember(t *testing.T, s *Store, update, chat, user int64, old, next string, snap JoinSnapshot) int64 {
	t.Helper()
	in, m := captureMember(t, s, update, chat, user, old, next)
	id, err := s.AddJoinEvent(context.Background(), in, m, snap, "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func joinResult(p, money, sexual float64) decision.Result {
	return decision.Result{Request: []byte(`{"state":{"member":{"name":"member"}}}`), Response: []byte(`{"raw":"model-response"}`), Probabilities: map[string]float64{"join.promotion": p, "join.money": money, "join.sexual": sexual, "join.earnings": 1}}
}

func claimJoin(t *testing.T, s *Store) *JoinJob {
	t.Helper()
	j, err := s.ClaimJoin(context.Background(), testNow)
	if err != nil || j == nil {
		t.Fatalf("claim join=%+v %v", j, err)
	}
	return j
}

func TestJoinArchiveDeduplicatesAndKeepsQueueIndependent(t *testing.T) {
	s := openTest(t)
	snap := joinSnapshot("record")
	snap.Policy.QueueLimit = 2
	first := addMember(t, s, 1, -100, 50, "left", "member", snap)
	if again := addMember(t, s, 1, -100, 50, "left", "member", snap); again != first {
		t.Fatal("duplicate arrival created work")
	}
	addMember(t, s, 2, -100, 51, "left", "member", snap)
	addMember(t, s, 3, -100, 52, "left", "member", snap)
	addMember(t, s, 4, -200, 53, "left", "member", snap)
	stats, err := s.JoinStats(context.Background(), -100)
	if err != nil {
		t.Fatal(err)
	}
	if stats["evicted"] != 1 || stats["pending"] != 2 {
		t.Fatalf("queue=%v", stats)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM updates WHERE processed=1 AND json_extract(raw,'$.extra')='retained'`).Scan(&count)
	if count != 4 {
		t.Fatal("raw updates were lost")
	}
	s.db.QueryRow(`SELECT count(*) FROM revisions`).Scan(&count)
	if count != 0 {
		t.Fatal("join events entered message history")
	}
	snap.Policy.Enabled = false
	addMember(t, s, 5, -100, 54, "left", "member", snap)
	stats, _ = s.JoinStats(context.Background(), -100)
	if stats["disabled"] != 1 {
		t.Fatal("disabled gate enqueued member")
	}
}

func TestJoinRetryFreezesProfileAndKeepsFailureLocal(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	snap := joinSnapshot("enforce")
	snap.Policy.QueueLimit = 1
	addMember(t, s, 1, -100, 50, "left", "member", snap)
	j := claimJoin(t, s)
	profile := telegram.JoinProfileLookup{Profile: telegram.Profile{Status: "available", Bio: "first", FetchedAt: 1}}
	state, err := s.FreezeJoinContext(ctx, j.ID, profile, []byte(`{"member":{"name":"first"}}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.FreezeJoinContext(ctx, j.ID, telegram.JoinProfileLookup{}, []byte(`{"changed":true}`))
	if err != nil || string(second) != string(state) {
		t.Fatal("context was replaced")
	}
	if _, err := s.FinishJoin(ctx, *j, joinResult(1, 1, 0), errors.New("offline"), testNow, 10); err != nil {
		t.Fatal(err)
	}
	if j, err := s.ClaimJoin(ctx, testNow.Add(9*time.Second)); err != nil || j != nil {
		t.Fatal("join retry backoff ignored")
	}
	add(t, s, 20, -100, 1, 50, config.Defaults().Base)
	if j, err := s.Claim(ctx, testNow); err != nil || j == nil {
		t.Fatal("join retry paused message queue")
	}
	addMember(t, s, 2, -100, 51, "left", "member", snap)
	if _, err := s.FinishJoin(ctx, *j, joinResult(1, 1, 0), nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM join_actions`).Scan(&n)
	if n != 0 {
		t.Fatal("late result resurrected evicted job")
	}
	s.db.QueryRow(`SELECT count(*) FROM join_attempts WHERE error='offline' AND response<>''`).Scan(&n)
	if n != 1 {
		t.Fatal("failure evidence lost")
	}
	s.db.QueryRow(`SELECT count(*) FROM join_events WHERE json_extract(profile,'$.bio')='first'`).Scan(&n)
	if n != 1 {
		t.Fatal("profile snapshot lost")
	}
}

func TestJoinScoringPlansOnlyCurrentEpisodeAndMode(t *testing.T) {
	for _, tc := range []struct {
		mode             string
		p, money, sexual float64
		want             int
	}{{"record", 1, 1, 0, 0}, {"enforce", .849, 1, 1, 0}, {"enforce", .85, .85, 0, 1}, {"enforce", .85, 0, .85, 1}, {"enforce", 1, 0, 0, 0}} {
		s := openTest(t)
		ctx := context.Background()
		addMember(t, s, 1, -100, 50, "left", "member", joinSnapshot(tc.mode))
		j := claimJoin(t, s)
		r, err := s.FinishJoin(ctx, *j, joinResult(tc.p, tc.money, tc.sexual), nil, testNow, 10)
		if err != nil {
			t.Fatal(err)
		}
		a, err := s.TakeJoinAction(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if (a != nil) != (tc.want == 1) {
			t.Fatalf("case=%+v report=%+v action=%+v", tc, r, a)
		}
		if tc.mode == "record" {
			var n int
			s.db.QueryRow(`SELECT count(*) FROM join_actions WHERE status='recorded'`).Scan(&n)
			if n != 1 {
				t.Fatal("record-mode plan lost")
			}
		}
	}
	s := openTest(t)
	ctx := context.Background()
	snap := joinSnapshot("enforce")
	addMember(t, s, 1, -100, 50, "left", "member", snap)
	old := claimJoin(t, s)
	addMember(t, s, 2, -100, 50, "member", "left", snap)
	addMember(t, s, 3, -100, 50, "left", "member", snap)
	if _, err := s.FinishJoin(ctx, *old, joinResult(1, 1, 0), nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.TakeJoinAction(ctx); a != nil {
		t.Fatal("old episode punished rejoined member")
	}
	j := claimJoin(t, s)
	if _, err := s.FinishJoin(ctx, *j, joinResult(1, 1, 0), nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
	a, err := s.TakeJoinAction(ctx)
	if err != nil || a == nil {
		t.Fatal("new episode lost its own decision")
	}
	// A durably captured departure must block dispatch even before inbox processing.
	captureMember(t, s, 4, -100, 50, "member", "left")
	if ready, err := s.StartJoinAction(ctx, *a); err != nil || ready {
		t.Fatal("unprocessed departure allowed removal")
	}
}

func TestJoinCancellationStopsInflightResultsAndClaimedActions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	snap := joinSnapshot("enforce")
	addMember(t, s, 1, -100, 50, "left", "member", snap)
	j := claimJoin(t, s)
	if err := s.CancelJoin(ctx, -100, "gate disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishJoin(ctx, *j, joinResult(1, 1, 0), nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.TakeJoinAction(ctx); a != nil {
		t.Fatal("off revived an in-flight result")
	}
	addMember(t, s, 2, -100, 51, "left", "member", snap)
	j = claimJoin(t, s)
	if _, err := s.FinishJoin(ctx, *j, joinResult(1, 1, 0), nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
	a, err := s.TakeJoinAction(ctx)
	if err != nil || a == nil {
		t.Fatal(err)
	}
	if err := s.CancelJoin(ctx, -100, "gate disabled"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.StartJoinAction(ctx, *a); err != nil || ok {
		t.Fatal("off failed to cancel preflight action")
	}
}

func TestJoinRestartRetriesInferenceAndQuarantinesDispatchedActions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snap := joinSnapshot("enforce")
	addMember(t, s, 1, -100, 50, "left", "member", snap)
	j := claimJoin(t, s)
	if _, err := s.FinishJoin(ctx, *j, joinResult(1, 1, 0), nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
	a, err := s.TakeJoinAction(ctx)
	if err != nil || a == nil {
		t.Fatal(err)
	}
	if ready, err := s.StartJoinAction(ctx, *a); err != nil || !ready {
		t.Fatalf("dispatch=%v %v", ready, err)
	}
	addMember(t, s, 2, -100, 51, "left", "member", snap)
	claimJoin(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if j := claimJoin(t, s); j.UserID != 51 {
		t.Fatal("inference was not recovered")
	}
	if a, err := s.TakeJoinAction(ctx); err != nil || a != nil {
		t.Fatal("uncertain action was replayed")
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM join_actions WHERE status='uncertain'`).Scan(&n)
	if n != 1 {
		t.Fatal("unknown outcome lost")
	}
}

func TestPendingMemberPermissionChangePreservesJoinEpisode(t *testing.T) {
	for _, next := range []string{"member", "restricted"} {
		t.Run(next, func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			snap := joinSnapshot("enforce")
			addMember(t, s, 1, -100, 50, "left", "member", snap)
			j := claimJoin(t, s)
			raw := []byte(fmt.Sprintf(`{"update_id":2,"chat_member":{"chat":{"id":-100,"type":"supergroup"},"date":%d,"old_chat_member":{"status":"member","user":{"id":50}},"new_chat_member":{"status":%q,"is_member":true,"user":{"id":50}}}}`, testNow.Unix()+2, next))
			if err := s.Capture(ctx, []json.RawMessage{raw}); err != nil {
				t.Fatal(err)
			}
			if current, err := s.JoinCurrent(ctx, j.ID); err != nil || !current {
				t.Fatalf("same-episode pending update terminated screening: %v %v", current, err)
			}
			if _, err := s.FinishJoin(ctx, *j, joinResult(1, 1, 0), nil, testNow, 10); err != nil {
				t.Fatal(err)
			}
			if a, err := s.TakeJoinAction(ctx); err != nil || a == nil {
				t.Fatal("permission update lost the join decision")
			}
		})
	}
}

func TestJoinDeletionRuleCannotBecomeMemberKick(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	snap := joinSnapshot("enforce")
	snap.Policy.Rules["delete_join_message"] = config.Rule{Enabled: true, Count: 1, Action: "delete"}
	addMember(t, s, 1, -100, 50, "left", "member", snap)
	j := claimJoin(t, s)
	if _, err := s.FinishJoin(ctx, *j, joinResult(1, 1, 0), nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
	a, err := s.TakeJoinAction(ctx)
	if err != nil || a == nil || a.RuleID != "remove" || a.Rule.Action != "kick" {
		t.Fatalf("deletion rule became member removal: %+v %v", a, err)
	}
	snap.Policy.Rules["remove"] = config.Rule{Enabled: false, Count: 1, Action: "kick"}
	addMember(t, s, 2, -100, 51, "left", "member", snap)
	j = claimJoin(t, s)
	if _, err := s.FinishJoin(ctx, *j, joinResult(1, 1, 0), nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
	if a, err := s.TakeJoinAction(ctx); err != nil || a != nil {
		t.Fatal("deletion-only policy removed a member")
	}
}
