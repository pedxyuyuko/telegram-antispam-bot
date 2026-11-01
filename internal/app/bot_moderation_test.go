package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"telegram-antispam-bot/internal/store"
	"telegram-antispam-bot/internal/telegram"
)

type botProbe struct {
	calls   atomic.Int32
	mu      sync.Mutex
	deleted []int
}

func (p *botProbe) deletes() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.deleted...)
}
func botFixture(t *testing.T, role, mode string, observers ...func(*http.Request)) (*App, string, *botProbe) {
	t.Helper()
	probe := &botProbe{}
	a, path := appFixture(t, role, func(r *http.Request) {
		for _, observe := range observers {
			observe(r)
		}
		if strings.HasSuffix(r.URL.Path, "deleteMessage") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			id, _ := strconv.Atoi(r.FormValue("message_id"))
			probe.mu.Lock()
			probe.deleted = append(probe.deleted, id)
			probe.mu.Unlock()
		}
	})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probe.calls.Add(1)
		var req struct {
			Model string `json:"model"`
			State struct {
				Sender struct {
					IsBot bool `json:"is_bot"`
				} `json:"sender"`
			} `json:"state"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		p := .01
		if req.State.Sender.IsBot {
			p = .99
		}
		answers := map[string]any{}
		for id := range req.Questions {
			answers[id] = map[string]any{"type": "noul", "noul": p}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "answers": answers})
	}))
	t.Cleanup(model.Close)
	c := a.Config.Snapshot()
	c.Model.BaseURL = model.URL
	c.Base.Mode = mode
	saveScoringConfig(t, a, path, c)
	return a, path, probe
}
func guestReply(caller *telegram.Message, id int, bot int64) *telegram.Message {
	m := message(bot, "bot advertisement")
	m.ID = id
	m.Chat = caller.Chat
	m.From.IsBot = true
	m.From.FirstName = "Ad bot"
	m.From.Username = "ad_bot"
	m.Reply = caller
	m.GuestBotCallerUser = &telegram.User{ID: caller.UserID(), FirstName: "Caller"}
	return m
}
func doAction(t *testing.T, a *App) {
	t.Helper()
	if worked, err := a.ProcessAction(context.Background()); err != nil || !worked {
		t.Fatalf("action: %v %v", worked, err)
	}
}

func TestGuestAdDeletionLearnsBotAndDeletesZeroScoreCaller(t *testing.T) {
	a, path, probe := botFixture(t, "member", "enforce")
	caller := message(50, "/start 720 @ad_bot")
	deliver(t, a, 1, caller)
	runModel(t, a, time.Now())
	deliver(t, a, 2, guestReply(caller, 2, 200))
	runModel(t, a, time.Now())
	doAction(t, a)
	entry, err := a.Store.BotBlacklist(context.Background(), 200)
	if err != nil || entry == nil || !entry.Active {
		t.Fatalf("successful bot deletion not learned: %+v %v", entry, err)
	}
	doAction(t, a)
	got := probe.deletes()
	if len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("delete targets=%v want bot then caller", got)
	}
	if scalar(t, path, `SELECT count(*) FROM actions WHERE message_id=1 AND kind='delete' AND status='succeeded'`) != 1 {
		t.Fatal("caller delete not audited")
	}
	if scalar(t, path, `SELECT count(*) FROM attempts a JOIN revisions r ON r.id=a.revision_id WHERE r.message_id=1 AND json_extract(a.probabilities,'$.violation')=0.01`) != 1 {
		t.Fatal("caller's original model result changed")
	}
}

func TestGuestAdRecordModeKeepsLinkedIntentsWithoutLearning(t *testing.T) {
	a, path, probe := botFixture(t, "member", "record")
	caller := message(50, "/start 720 @ad_bot")
	deliver(t, a, 1, caller)
	runModel(t, a, time.Now())
	deliver(t, a, 2, guestReply(caller, 2, 200))
	runModel(t, a, time.Now())
	if scalar(t, path, `SELECT count(*) FROM actions WHERE kind='delete' AND status='recorded'`) != 2 {
		t.Fatal("record mode lost linked intent")
	}
	if entry, err := a.Store.BotBlacklist(context.Background(), 200); err != nil || entry != nil {
		t.Fatalf("record-only result entered blacklist: %+v %v", entry, err)
	}
	if worked, err := a.ProcessAction(context.Background()); err != nil || worked || len(probe.deletes()) != 0 {
		t.Fatal("record mode dispatched moderation")
	}
}

func TestGlobalBlacklistBypassesModelAndSupportsOtherGroups(t *testing.T) {
	for _, mode := range []string{"record", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			a, path, probe := botFixture(t, "member", mode)
			if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: 200, Username: "old_name_bot"}, true, 9, 9001); err != nil {
				t.Fatal(err)
			}
			m := message(200, "new content")
			m.ID = 10
			m.Chat.ID = -200
			m.From.IsBot = true
			m.From.Username = "renamed_bot"
			deliver(t, a, 1, m)
			if worked, err := a.ProcessModel(context.Background(), time.Now()); err != nil || worked {
				t.Fatalf("blacklisted bot reached model: %v %v", worked, err)
			}
			if probe.calls.Load() != 0 || scalar(t, path, `SELECT count(*) FROM attempts`) != 0 {
				t.Fatal("fabricated inference for blacklist hit")
			}
			if scalar(t, path, `SELECT count(*) FROM blacklist_matches`) != 1 {
				t.Fatal("blacklist match not audited")
			}
			if mode == "enforce" {
				doAction(t, a)
				if len(probe.deletes()) != 1 {
					t.Fatal("known bot not deleted")
				}
			} else if scalar(t, path, `SELECT count(*) FROM actions WHERE status='recorded'`) != 1 {
				t.Fatal("record-only blacklist action lost")
			}
		})
	}
}

func TestBlacklistRemovalCancelsPendingDeletion(t *testing.T) {
	a, path, probe := botFixture(t, "member", "enforce")
	ctx := context.Background()
	if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
		t.Fatal(err)
	}
	m := message(200, "known bot")
	m.From.IsBot = true
	deliver(t, a, 1, m)
	if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: 200}, false, 9, 9002); err != nil {
		t.Fatal(err)
	}
	doAction(t, a)
	if len(probe.deletes()) != 0 || scalar(t, path, `SELECT count(*) FROM actions WHERE status='skipped'`) != 1 {
		t.Fatal("removed blacklist entry still punished")
	}
}

func TestBlacklistedGuestRecordsBothDeletionIntents(t *testing.T) {
	a, path, _ := botFixture(t, "member", "record")
	if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
		t.Fatal(err)
	}
	caller := message(50, "/start 1 @ad_bot")
	deliver(t, a, 1, caller)
	runModel(t, a, time.Now())
	deliver(t, a, 2, guestReply(caller, 2, 200))
	if scalar(t, path, `SELECT count(*) FROM actions WHERE kind='delete' AND status='recorded'`) != 2 {
		t.Fatal("blacklisted guest lost recorded caller deletion")
	}
}

func TestOrdinaryBotRepliesDoNotDeleteTheRepliedUser(t *testing.T) {
	a, path, probe := botFixture(t, "member", "enforce")
	caller := message(50, "ordinary discussion")
	deliver(t, a, 1, caller)
	runModel(t, a, time.Now())
	bot := guestReply(caller, 2, 200)
	bot.GuestBotCallerUser = nil
	deliver(t, a, 2, bot)
	runModel(t, a, time.Now())
	doAction(t, a)
	if len(probe.deletes()) != 1 || scalar(t, path, `SELECT count(*) FROM bot_invocations`) != 0 {
		t.Fatal("plain reply was treated as invocation")
	}
}

func TestEditedInvocationIsProtectedBeforeAndAfterPlanning(t *testing.T) {
	for _, afterPlan := range []bool{false, true} {
		t.Run(strconv.FormatBool(afterPlan), func(t *testing.T) {
			a, path, probe := botFixture(t, "member", "enforce")
			caller := message(50, "/start 1 @ad_bot")
			deliver(t, a, 1, caller)
			runModel(t, a, time.Now())
			deliver(t, a, 2, guestReply(caller, 2, 200))
			runModel(t, a, time.Now())
			if afterPlan {
				doAction(t, a)
			}
			edited := message(50, "ordinary edited discussion")
			edited.EditDate = caller.Date + 1
			deliver(t, a, 3, edited)
			doAction(t, a)
			if len(probe.deletes()) != 1 || probe.deletes()[0] != 2 {
				t.Fatal("edited caller was deleted")
			}
			if scalar(t, path, `SELECT count(*) FROM actions WHERE message_id=1 AND status='succeeded'`) != 0 {
				t.Fatal("edited target action succeeded")
			}
		})
	}
}

func TestMultipleBotRepliesDeleteInvocationOnce(t *testing.T) {
	a, path, probe := botFixture(t, "member", "enforce")
	caller := message(50, "/start 1 @first_bot @second_bot @third_bot")
	deliver(t, a, 1, caller)
	runModel(t, a, time.Now())
	for i := 0; i < 3; i++ {
		deliver(t, a, int64(i+2), guestReply(caller, i+2, int64(200+i)))
		runModel(t, a, time.Now())
	}
	for i := 0; i < 4; i++ {
		doAction(t, a)
	}
	if len(probe.deletes()) != 4 || scalar(t, path, `SELECT count(*) FROM actions WHERE message_id=1 AND kind='delete'`) != 1 {
		t.Fatal("caller deletion duplicated")
	}
	if scalar(t, path, `SELECT count(*) FROM bot_invocations WHERE target_action_id IS NOT NULL`) != 3 {
		t.Fatal("bot evidence links were lost")
	}
}

func TestLateCallerArchiveResolvesPendingInvocationEvidence(t *testing.T) {
	a, path, probe := botFixture(t, "member", "enforce")
	caller := message(50, "/start 1 @ad_bot")
	deliver(t, a, 1, guestReply(caller, 2, 200))
	runModel(t, a, time.Now())
	doAction(t, a)
	if scalar(t, path, `SELECT count(*) FROM revisions WHERE message_id=1`) != 0 {
		t.Fatal("fabricated original caller archive")
	}
	deliver(t, a, 2, caller)
	doAction(t, a)
	if len(probe.deletes()) != 2 || probe.deletes()[1] != 1 {
		t.Fatal("late invocation did not resolve")
	}
}

func TestOwnerRemovalWinsOverAlreadyQueuedModelEvidence(t *testing.T) {
	a, _, _ := botFixture(t, "member", "enforce")
	ctx := context.Background()
	bot := message(200, "advertisement")
	bot.From.IsBot = true
	deliver(t, a, 1, bot)
	runModel(t, a, time.Now())
	if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: 200}, false, 9, 9001); err != nil {
		t.Fatal(err)
	}
	doAction(t, a)
	entry, err := a.Store.BotBlacklist(ctx, 200)
	if err != nil || entry == nil || entry.Active {
		t.Fatal("old model evidence undid owner removal")
	}
	bot.ID = 2
	deliver(t, a, 2, bot)
	runModel(t, a, time.Now())
	doAction(t, a)
	entry, err = a.Store.BotBlacklist(ctx, 200)
	if err != nil || entry == nil || !entry.Active {
		t.Fatal("new confirmed advertisement was not learned")
	}
}

func TestBlacklistRespectsAdministratorOwnerAndMembershipProtection(t *testing.T) {
	for _, tc := range []struct {
		name, role string
		id         int64
	}{{"administrator", "administrator", 200}, {"unavailable", "", 200}, {"owner", "member", 9}, {"own_bot", "member", 123}} {
		t.Run(tc.name, func(t *testing.T) {
			a, path, probe := botFixture(t, tc.role, "enforce")
			if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: tc.id}, true, 9, 9001); err != nil {
				t.Fatal(err)
			}
			bot := message(tc.id, "known bot")
			bot.From.IsBot = true
			deliver(t, a, 1, bot)
			if _, err := a.ProcessAction(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(probe.deletes()) != 0 || probe.calls.Load() != 0 {
				t.Fatal("protected sender was acted on")
			}
			if scalar(t, path, `SELECT count(*) FROM revisions`) != 1 {
				t.Fatal("protected raw message lost")
			}
		})
	}
}

func TestBlacklistedMessagesStayOutsideOutageQueue(t *testing.T) {
	a, path, probe := botFixture(t, "member", "enforce")
	if err := a.Config.Set(nil, "queue_limit", json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
		t.Fatal(err)
	}
	deliver(t, a, 1, message(50, "waiting for model"))
	bot := message(200, "known bot")
	bot.ID = 2
	bot.From.IsBot = true
	deliver(t, a, 2, bot)
	if scalar(t, path, `SELECT count(*) FROM revisions WHERE status='pending' AND message_id=1`) != 1 || scalar(t, path, `SELECT count(*) FROM revisions WHERE status='evicted'`) != 0 {
		t.Fatal("blacklisted message displaced pending model work")
	}
	doAction(t, a)
	if probe.calls.Load() != 0 || len(probe.deletes()) != 1 {
		t.Fatal("blacklist needed the model queue")
	}
}

func TestBlacklistRespectsDisabledDeleteRule(t *testing.T) {
	a, path, probe := botFixture(t, "member", "enforce")
	if err := a.Config.Set(nil, "rules.delete.enabled", json.RawMessage(`false`)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
		t.Fatal(err)
	}
	bot := message(200, "known bot")
	bot.From.IsBot = true
	deliver(t, a, 1, bot)
	if scalar(t, path, `SELECT count(*) FROM actions`) != 0 || probe.calls.Load() != 0 {
		t.Fatal("disabled deletion rule was bypassed")
	}
}

func TestRecordedSourceCannotCreateEnforcementAfterModeChange(t *testing.T) {
	a, path, probe := botFixture(t, "member", "enforce")
	caller := message(50, "/start 1 @ad_bot")
	deliver(t, a, 1, caller)
	runModel(t, a, time.Now())
	if err := a.Config.Set(nil, "mode", json.RawMessage(`"record"`)); err != nil {
		t.Fatal(err)
	}
	deliver(t, a, 2, guestReply(caller, 2, 200))
	runModel(t, a, time.Now())
	if err := a.Config.Set(nil, "mode", json.RawMessage(`"enforce"`)); err != nil {
		t.Fatal(err)
	}
	if worked, err := a.ProcessAction(context.Background()); err != nil || worked {
		t.Fatal("recorded source created enforceable work")
	}
	if len(probe.deletes()) != 0 || scalar(t, path, `SELECT count(*) FROM actions WHERE status='recorded'`) != 2 {
		t.Fatal("record/enforce boundary lost")
	}
}

func TestBlacklistGenerationCancelsWorkFromEarlierMembership(t *testing.T) {
	a, path, probe := botFixture(t, "member", "enforce")
	ctx := context.Background()
	if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
		t.Fatal(err)
	}
	bot := message(200, "old known bot message")
	bot.From.IsBot = true
	deliver(t, a, 1, bot)
	if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: 200}, false, 9, 9002); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: 200}, true, 9, 9003); err != nil {
		t.Fatal(err)
	}
	doAction(t, a)
	bot.ID = 2
	deliver(t, a, 2, bot)
	doAction(t, a)
	if got := probe.deletes(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("stale membership action executed: %v", got)
	}
	if scalar(t, path, `SELECT count(*) FROM actions WHERE status='skipped'`) != 1 {
		t.Fatal("stale action outcome lost")
	}
}

func TestBlacklistReplyCanCleanUpAnEvictedInvocation(t *testing.T) {
	a, path, probe := botFixture(t, "member", "enforce")
	if err := a.Config.Set(nil, "queue_limit", json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
		t.Fatal(err)
	}
	caller := message(50, "/start 1 @ad_bot")
	deliver(t, a, 1, caller)
	newer := message(51, "waiting for model")
	newer.ID = 2
	deliver(t, a, 2, newer)
	if scalar(t, path, `SELECT count(*) FROM revisions WHERE message_id=1 AND status='evicted'`) != 1 {
		t.Fatal("fixture did not evict caller")
	}
	deliver(t, a, 3, guestReply(caller, 3, 200))
	doAction(t, a)
	doAction(t, a)
	if got := probe.deletes(); len(got) != 2 || got[0] != 3 || got[1] != 1 {
		t.Fatalf("known guest bot left evicted invocation visible: %v", got)
	}
	if probe.calls.Load() != 0 || scalar(t, path, `SELECT count(*) FROM revisions WHERE message_id=1 AND status='evicted'`) != 1 {
		t.Fatal("caller was reinserted into model work")
	}
}

func TestLateInvocationUsesConfirmedEnforceEvidenceAfterRecordedEvidence(t *testing.T) {
	a, path, probe := botFixture(t, "member", "record")
	caller := message(50, "/start 1 @first_bot @second_bot")
	deliver(t, a, 1, guestReply(caller, 2, 200))
	runModel(t, a, time.Now())
	if err := a.Config.Set(nil, "mode", json.RawMessage(`"enforce"`)); err != nil {
		t.Fatal(err)
	}
	deliver(t, a, 2, guestReply(caller, 3, 201))
	runModel(t, a, time.Now())
	doAction(t, a)
	deliver(t, a, 3, caller)
	doAction(t, a)
	if got := probe.deletes(); len(got) != 2 || got[0] != 3 || got[1] != 1 {
		t.Fatalf("confirmed evidence did not enable caller cleanup: %v", got)
	}
	if scalar(t, path, `SELECT count(*) FROM actions WHERE message_id=1 AND mode='enforce' AND status='succeeded'`) != 1 {
		t.Fatal("caller action kept stale recorded mode")
	}
	if scalar(t, path, `SELECT count(*) FROM bot_action_events e JOIN actions a ON a.id=e.action_id WHERE a.message_id=1 AND e.event='before_upgrade' AND e.mode='record' AND e.status='recorded'`) != 1 {
		t.Fatal("original recorded intent was not preserved in audit")
	}
}

func TestInvocationCanUseAnyStillValidConfirmedSource(t *testing.T) {
	for _, removed := range []int64{200, 201} {
		t.Run(strconv.FormatInt(removed, 10), func(t *testing.T) {
			a, path, probe := botFixture(t, "member", "enforce")
			ctx := context.Background()
			for i, id := range []int64{200, 201} {
				if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: id}, true, 9, int64(9001+i)); err != nil {
					t.Fatal(err)
				}
			}
			caller := message(50, "/start 1 @first_bot @second_bot")
			deliver(t, a, 1, caller)
			runModel(t, a, time.Now())
			deliver(t, a, 2, guestReply(caller, 2, 200))
			deliver(t, a, 3, guestReply(caller, 3, 201))
			doAction(t, a)
			doAction(t, a)
			if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: removed}, false, 9, 9003); err != nil {
				t.Fatal(err)
			}
			doAction(t, a)
			if got := probe.deletes(); len(got) != 3 || got[2] != 1 {
				t.Fatalf("valid source was pinned behind removed source: %v", got)
			}
			if scalar(t, path, `SELECT count(*) FROM action_evidence e JOIN actions a ON a.id=e.action_id WHERE a.message_id=1 AND e.bot_id=`+strconv.FormatInt(401-removed, 10)) != 1 {
				t.Fatal("selected valid source was not audited")
			}
			if scalar(t, path, `SELECT count(*) FROM bot_action_events e JOIN actions a ON a.id=e.action_id WHERE a.message_id=1 AND e.event='outcome' AND e.status='succeeded'`) != 1 {
				t.Fatal("linked deletion outcome audit missing")
			}
		})
	}
}

func TestUnbanDuringMembershipLookupCancelsUndispatchedBotWork(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(strconv.FormatBool(linked), func(t *testing.T) {
			var block atomic.Bool
			var deletes atomic.Int32
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			a, path := appFixture(t, "member", func(r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "deleteMessage") {
					deletes.Add(1)
				}
				if strings.HasSuffix(r.URL.Path, "getChatMember") && block.Load() {
					select {
					case entered <- struct{}{}:
					default:
					}
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}
			})
			if err := a.Config.Set(nil, "mode", json.RawMessage(`"enforce"`)); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
				t.Fatal(err)
			}
			if linked {
				caller := message(50, "/start 1 @ad_bot")
				deliver(t, a, 1, caller)
				deliver(t, a, 2, guestReply(caller, 2, 200))
				doAction(t, a)
			} else {
				bot := message(200, "known bot")
				bot.From.IsBot = true
				deliver(t, a, 1, bot)
			}
			before := deletes.Load()
			block.Store(true)
			done := make(chan error, 1)
			go func() { _, err := a.ProcessAction(context.Background()); done <- err }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("membership query did not start")
			}
			if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: 200}, false, 9, 9002); err != nil {
				t.Fatal(err)
			}
			once.Do(func() { close(release) })
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("action did not finish")
			}
			if deletes.Load() != before || scalar(t, path, `SELECT count(*) FROM actions WHERE status='skipped'`) != 1 {
				t.Fatal("removed generation dispatched after membership lookup")
			}
		})
	}
}
