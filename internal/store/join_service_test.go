package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/telegram"
)

func addJoinService(t *testing.T, s *Store, update, chat, date int64, users []int64, snap JoinSnapshot) int64 {
	t.Helper()
	m := &telegram.Message{ID: int(update), Chat: telegram.Chat{ID: chat, Type: "supergroup"}, Date: date, From: &telegram.User{ID: 99, FirstName: "Inviter"}}
	for _, id := range users {
		m.NewChatMembers = append(m.NewChatMembers, telegram.User{ID: id, FirstName: "Joined"})
	}
	raw, _ := json.Marshal(map[string]any{"update_id": update, "message": m})
	ctx := context.Background()
	if err := s.Capture(ctx, []json.RawMessage{raw}); err != nil {
		t.Fatal(err)
	}
	report, err := s.AddRevision(ctx, Inbox{ID: update, Raw: raw, ReceivedAt: testNow.UnixMilli()}, m, Snapshot{Policy: config.Defaults().Base, Questions: config.Defaults().Questions, Join: &snap, SkipReason: "membership_service"}, "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkInbox(ctx, update, ""); err != nil {
		t.Fatal(err)
	}
	return report.ID
}

func finishJoinForService(t *testing.T, s *Store, spam bool) {
	t.Helper()
	p := 0.0
	if spam {
		p = 1
	}
	j := claimJoin(t, s)
	if _, err := s.FinishJoin(context.Background(), *j, joinResult(p, p, 0), nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
}

func serviceConfig(snap JoinSnapshot) config.Config {
	c := config.Defaults()
	c.JoinGate = &config.JoinGate{Base: snap.Policy, Groups: map[string]json.RawMessage{}}
	c.QuestionGroups = map[string]map[string]config.Question{"join": {}}
	for id, q := range snap.Questions {
		c.QuestionGroups["join"][id[len("join."):]] = q
	}
	return c
}

func TestJoinServiceDeletionLinksEitherArrivalOrderAndPreservesOriginal(t *testing.T) {
	for _, late := range []bool{false, true} {
		s := openTest(t)
		ctx := context.Background()
		snap := joinSnapshot("enforce")
		var revision int64
		if !late {
			revision = addJoinService(t, s, 10, -100, testNow.Unix()+1, []int64{50}, snap)
		}
		addMember(t, s, 1, -100, 50, "left", "member", snap)
		finishJoinForService(t, s, true)
		if late {
			revision = addJoinService(t, s, 10, -100, testNow.Unix()+1, []int64{50}, snap)
		}
		a, err := s.TakeJoinServiceAction(ctx)
		if err != nil || a == nil || a.RevisionID != revision || a.MessageID != 10 || len(a.Evidence) != 1 {
			t.Fatalf("late=%v cleanup=%+v %v", late, a, err)
		}
		if ready, err := s.StartJoinServiceAction(ctx, *a, serviceConfig(snap)); err != nil || !ready {
			t.Fatalf("cleanup dispatch=%v %v", ready, err)
		}
		if err := s.CompleteJoinServiceAction(ctx, a.ID, "succeeded", ""); err != nil {
			t.Fatal(err)
		}
		addJoinService(t, s, 10, -100, testNow.Unix()+1, []int64{50}, snap)
		if a, err := s.TakeJoinServiceAction(ctx); err != nil || a != nil {
			t.Fatal("duplicate update repeated cleanup")
		}
		j, err := s.Revision(ctx, revision)
		if err != nil || j.Status != "ignored" || len(j.Raw) == 0 || j.Snapshot.Join == nil {
			t.Fatal("service original or receipt policy lost")
		}
	}
}

func TestJoinServiceRequiresAllNamedMembersToMatchSameEvent(t *testing.T) {
	for _, tc := range []struct {
		name               string
		users              []int64
		chat, date         int64
		secondSpam, expect bool
	}{
		{"all_spam", []int64{50, 51}, -100, 1, true, true},
		{"mixed", []int64{50, 51}, -100, 1, false, false},
		{"unknown", []int64{50, 52}, -100, 1, true, false},
		{"wrong_chat", []int64{50}, -200, 1, true, false},
		{"wrong_date", []int64{50}, -100, 2, true, false},
		{"wrong_user", []int64{99}, -100, 1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			snap := joinSnapshot("enforce")
			addJoinService(t, s, 10, tc.chat, testNow.Unix()+tc.date, tc.users, snap)
			addMember(t, s, 1, -100, 50, "left", "member", snap)
			finishJoinForService(t, s, true)
			// Two members can share one official service message and timestamp.
			in, m := captureMember(t, s, 2, -100, 51, "left", "member", testNow.Unix()+1)
			if _, err := s.AddJoinEvent(ctx, in, m, snap, ""); err != nil {
				t.Fatal(err)
			}
			finishJoinForService(t, s, tc.secondSpam)
			a, err := s.TakeJoinServiceAction(ctx)
			if err != nil || (a != nil) != tc.expect {
				t.Fatalf("cleanup=%+v expected=%v %v", a, tc.expect, err)
			}
		})
	}
}

func TestJoinServiceRecordModeAndDisabledRulesNeverBecomeLive(t *testing.T) {
	for _, which := range []string{"service_record", "join_record", "service_off", "join_off", "rule_off"} {
		t.Run(which, func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			source, arrival := joinSnapshot("enforce"), joinSnapshot("enforce")
			switch which {
			case "service_record":
				source.Policy.Mode = "record"
			case "join_record":
				arrival.Policy.Mode = "record"
			case "service_off":
				source.Policy.Enabled = false
			case "join_off":
				arrival.Policy.Enabled = false
			case "rule_off":
				r := source.Policy.Rules["delete_join_message"]
				r.Enabled = false
				source.Policy.Rules["delete_join_message"] = r
			}
			addJoinService(t, s, 10, -100, testNow.Unix()+1, []int64{50}, source)
			addMember(t, s, 1, -100, 50, "left", "member", arrival)
			if arrival.Policy.Enabled {
				finishJoinForService(t, s, true)
			}
			if a, err := s.TakeJoinServiceAction(ctx); err != nil || a != nil {
				t.Fatal("record/disabled source produced a live deletion")
			}
			if which == "service_record" || which == "join_record" {
				var n int
				s.db.QueryRow(`SELECT count(*) FROM join_service_actions WHERE status='recorded'`).Scan(&n)
				if n != 1 {
					t.Fatal("record-mode cleanup plan lost")
				}
			}
		})
	}
}

func TestJoinServiceAmbiguousSameSecondRejoinBlocksDeletion(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	snap := joinSnapshot("enforce")
	addJoinService(t, s, 10, -100, testNow.Unix()+1, []int64{50}, snap)
	addMember(t, s, 1, -100, 50, "left", "member", snap)
	finishJoinForService(t, s, true)
	a, err := s.TakeJoinServiceAction(ctx)
	if err != nil || a == nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"update_id": 2, "chat_member": telegram.MembershipUpdate{Chat: telegram.Chat{ID: -100, Type: "supergroup"}, Date: testNow.Unix() + 1, Old: telegram.MemberState{Status: "left", User: telegram.User{ID: 50}}, New: telegram.MemberState{Status: "member", User: telegram.User{ID: 50}}}})
	if err := s.Capture(ctx, []json.RawMessage{raw}); err != nil {
		t.Fatal(err)
	}
	if ready, err := s.StartJoinServiceAction(ctx, *a, serviceConfig(snap)); err != nil || ready {
		t.Fatal("ambiguous captured rejoin permitted deletion")
	}
}

func TestJoinServiceDisableCancelsClaimedAndLateArrivingHints(t *testing.T) {
	for _, late := range []bool{false, true} {
		s := openTest(t)
		ctx := context.Background()
		snap := joinSnapshot("enforce")
		addMember(t, s, 1, -100, 50, "left", "member", snap)
		finishJoinForService(t, s, true)
		var claimed *JoinServiceAction
		if !late {
			addJoinService(t, s, 10, -100, testNow.Unix()+1, []int64{50}, snap)
			var err error
			claimed, err = s.TakeJoinServiceAction(ctx)
			if err != nil || claimed == nil {
				t.Fatal(err)
			}
		}
		if err := s.CancelJoin(ctx, -100, "gate disabled"); err != nil {
			t.Fatal(err)
		}
		if late {
			addJoinService(t, s, 10, -100, testNow.Unix()+1, []int64{50}, snap)
			if a, err := s.TakeJoinServiceAction(ctx); err != nil || a != nil {
				t.Fatal("off/on reactivated a late service message")
			}
		} else {
			if ready, err := s.StartJoinServiceAction(ctx, *claimed, serviceConfig(snap)); err != nil || ready {
				t.Fatal("off failed to cancel claimed deletion")
			}
		}
	}
}

func TestJoinServiceRestartPreservesUncertainDeleteAndOriginalTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snap := joinSnapshot("enforce")
	addJoinService(t, s, 10, -100, testNow.Unix()+1, []int64{50}, snap)
	addMember(t, s, 1, -100, 50, "left", "member", snap)
	finishJoinForService(t, s, true)
	a, err := s.TakeJoinServiceAction(ctx)
	if err != nil || a == nil {
		t.Fatal(err)
	}
	if ready, err := s.StartJoinServiceAction(ctx, *a, serviceConfig(snap)); err != nil || !ready {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if a, err := s.TakeJoinServiceAction(ctx); err != nil || a != nil {
		t.Fatal("uncertain deletion replayed")
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM join_service_actions WHERE status='uncertain'`).Scan(&n)
	if n != 1 {
		t.Fatal("uncertain outcome lost")
	}
	s.db.QueryRow(`SELECT count(*) FROM revisions WHERE raw<>''`).Scan(&n)
	if n != 1 {
		t.Fatal("service archive lost")
	}
}
