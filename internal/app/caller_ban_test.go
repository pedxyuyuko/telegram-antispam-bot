package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"telegram-antispam-bot/internal/store"
)

type banCall struct{ user, chat, until int64 }
type banProbe struct {
	mu    sync.Mutex
	calls []banCall
}

func TestCallerBanProtectsExistingMembershipAndHandlesDepartedCallers(t *testing.T) {
	for _, role := range []string{"member", "left", "restricted", "kicked", "administrator", "creator", ""} {
		t.Run(role, func(t *testing.T) {
			var membership atomic.Value
			membership.Store("member")
			probe := &banProbe{}
			a, path := appFixtureWithMembership(t, func() string { return membership.Load().(string) }, probe.observe)
			if err := a.Config.Set(nil, "mode", json.RawMessage(`"enforce"`)); err != nil {
				t.Fatal(err)
			}
			enableBan(t, a, 3600)
			if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
				t.Fatal(err)
			}
			caller := message(50, "@ad_bot")
			deliver(t, a, 1, caller)
			deliver(t, a, 2, guestReply(caller, 2, 200))
			doAction(t, a)
			membership.Store(role)
			drainActions(t, a, path)
			want := 0
			if role == "member" || role == "left" || role == "restricted" {
				want = 1
			}
			if len(probe.bans()) != want {
				t.Fatalf("membership=%q bans=%v", role, probe.bans())
			}
		})
	}
}

func TestCallerBanRechecksChangedPolicyEvidenceAndDeadline(t *testing.T) {
	for _, scenario := range []string{"disabled", "record", "duration_changed", "edited", "blacklist_removed", "owner", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			probe := &banProbe{}
			a, path, _ := botFixture(t, "member", "enforce", probe.observe)
			enableBan(t, a, 60)
			if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
				t.Fatal(err)
			}
			caller := message(50, "@ad_bot")
			deliver(t, a, 1, caller)
			deliver(t, a, 2, guestReply(caller, 2, 200))
			doAction(t, a)
			if scalar(t, path, `SELECT count(*) FROM actions WHERE kind='ban' AND status='pending'`) != 1 {
				t.Fatal("fixture missing planned ban")
			}
			switch scenario {
			case "disabled":
				if err := a.Config.Set(nil, "rules.ban.enabled", json.RawMessage(`false`)); err != nil {
					t.Fatal(err)
				}
			case "record":
				if err := a.Config.Set(nil, "mode", json.RawMessage(`"record"`)); err != nil {
					t.Fatal(err)
				}
			case "duration_changed":
				if err := a.Config.Set(nil, "rules.ban.duration_seconds", json.RawMessage(`-1`)); err != nil {
					t.Fatal(err)
				}
			case "edited":
				edited := *caller
				edited.Text = "ordinary discussion"
				edited.EditDate = caller.Date + 1
				deliver(t, a, 3, &edited)
			case "blacklist_removed":
				if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: 200}, false, 9, 9002); err != nil {
					t.Fatal(err)
				}
			case "owner":
				c := a.Config.Snapshot()
				c.OwnerIDs = append(c.OwnerIDs, 50)
				saveScoringConfig(t, a, path, c)
			case "expired":
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`UPDATE actions SET until_date=? WHERE kind='ban'`, time.Now().Unix()-1)
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			drainActions(t, a, path)
			if len(probe.bans()) != 0 || scalar(t, path, `SELECT count(*) FROM actions WHERE kind='ban' AND status='skipped'`) != 1 {
				t.Fatal("changed authority or expired deadline dispatched ban")
			}
		})
	}
}

func (p *banProbe) observe(r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/banChatMember") {
		return
	}
	r.ParseMultipartForm(1 << 20)
	user, _ := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	chat, _ := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
	until, _ := strconv.ParseInt(r.FormValue("until_date"), 10, 64)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, banCall{user, chat, until})
}
func (p *banProbe) bans() []banCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]banCall(nil), p.calls...)
}
func enableBan(t *testing.T, a *App, duration int) {
	t.Helper()
	if err := a.Config.Set(nil, "rules.ban.duration_seconds", json.RawMessage(strconv.Itoa(duration))); err != nil {
		t.Fatal(err)
	}
	if err := a.Config.Set(nil, "rules.ban.enabled", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
}
func drainActions(t *testing.T, a *App, path string) {
	t.Helper()
	for i := 0; i < 20; i++ {
		if scalar(t, path, `SELECT count(*) FROM actions WHERE status='pending'`) == 0 {
			return
		}
		if _, err := a.ProcessAction(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("action queue did not drain")
}

func TestCallerBanFollowsConfirmedAdDeletion(t *testing.T) {
	for _, tc := range []struct {
		mode           string
		enabled, known bool
		duration       int
	}{{"enforce", true, false, -1}, {"enforce", true, true, 3600}, {"enforce", false, true, -1}, {"record", true, false, -1}, {"record", true, true, -1}} {
		t.Run(fmt.Sprintf("%s/ban=%v/known=%v/duration=%d", tc.mode, tc.enabled, tc.known, tc.duration), func(t *testing.T) {
			probe := &banProbe{}
			a, path, deletes := botFixture(t, "member", tc.mode, probe.observe)
			if tc.enabled {
				enableBan(t, a, tc.duration)
			}
			if tc.known {
				if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
					t.Fatal(err)
				}
			}
			caller := message(50, "/start @ad_bot")
			deliver(t, a, 1, caller)
			runModel(t, a, time.Now())
			deliver(t, a, 2, guestReply(caller, 2, 200))
			if !tc.known {
				runModel(t, a, time.Now())
			}
			if tc.mode == "enforce" && scalar(t, path, `SELECT count(*) FROM actions WHERE kind='ban'`) != 0 {
				t.Fatal("ban planned before ad deletion succeeded")
			}
			drainActions(t, a, path)
			wantBans := 0
			wantDeletes := 0
			if tc.mode == "enforce" {
				wantDeletes = 2
				if tc.enabled {
					wantBans = 1
				}
			}
			calls := probe.bans()
			if len(calls) != wantBans || len(deletes.deletes()) != wantDeletes {
				t.Fatalf("bans=%v deletes=%v", calls, deletes.deletes())
			}
			if wantBans > 0 {
				c := calls[0]
				if c.user != 50 || c.chat != -100 {
					t.Fatalf("wrong caller banned: %+v", c)
				}
				if tc.duration == -1 && c.until != 0 {
					t.Fatal("permanent deadline lost")
				}
				if tc.duration > 0 && (c.until < time.Now().Unix()+3590 || c.until > time.Now().Unix()+3600) {
					t.Fatalf("finite deadline=%d", c.until)
				}
			}
			if tc.enabled {
				status := "recorded"
				if tc.mode == "enforce" {
					status = "succeeded"
				}
				if scalar(t, path, `SELECT count(*) FROM actions WHERE message_id=1 AND kind='ban' AND status='`+status+`'`) != 1 {
					t.Fatal("caller ban audit missing")
				}
				if scalar(t, path, `SELECT count(*) FROM bot_invocation_actions`) != 2 {
					t.Fatal("delete and ban evidence must both survive")
				}
			}
			if scalar(t, path, `SELECT count(*) FROM hits WHERE rule_id='ban'`) != 0 || scalar(t, path, `SELECT count(*) FROM revisions`) != 2 {
				t.Fatal("sender hit history or raw archive changed")
			}
		})
	}
}

func TestCallerBanRequiresOfficialCallerAndMatchingArchive(t *testing.T) {
	for _, scenario := range []string{"ordinary_reply", "different_user", "unarchived", "changed_text"} {
		t.Run(scenario, func(t *testing.T) {
			p := &banProbe{}
			a, path, _ := botFixture(t, "member", "enforce", p.observe)
			enableBan(t, a, -1)
			caller := message(50, "/start @ad_bot")
			if scenario != "unarchived" {
				deliver(t, a, 1, caller)
				runModel(t, a, time.Now())
			}
			ad := guestReply(caller, 2, 200)
			switch scenario {
			case "ordinary_reply":
				ad.GuestBotCallerUser = nil
			case "different_user":
				ad.GuestBotCallerUser.ID = 51
			case "changed_text":
				copy := *caller
				copy.Text = "unrelated"
				ad.Reply = &copy
			}
			deliver(t, a, 2, ad)
			runModel(t, a, time.Now())
			drainActions(t, a, path)
			if len(p.bans()) != 0 || scalar(t, path, `SELECT count(*) FROM actions WHERE kind='ban'`) != 0 {
				t.Fatal("untrusted caller punished")
			}
		})
	}
}

func TestCallerBanKeepsMultipleSourcesWithoutDuplicatePunishment(t *testing.T) {
	for _, removed := range []int64{200, 201} {
		t.Run(strconv.FormatInt(removed, 10), func(t *testing.T) {
			probe := &banProbe{}
			a, path, deletes := botFixture(t, "member", "enforce", probe.observe)
			enableBan(t, a, -1)
			ctx := context.Background()
			for i, id := range []int64{200, 201} {
				if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: id}, true, 9, int64(9001+i)); err != nil {
					t.Fatal(err)
				}
			}
			caller := message(50, "@first_bot @second_bot")
			deliver(t, a, 1, caller)
			deliver(t, a, 2, guestReply(caller, 2, 200))
			deliver(t, a, 3, guestReply(caller, 3, 201))
			doAction(t, a)
			doAction(t, a)
			if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: removed}, false, 9, 9003); err != nil {
				t.Fatal(err)
			}
			drainActions(t, a, path)
			if len(probe.bans()) != 1 || len(deletes.deletes()) != 3 || scalar(t, path, `SELECT count(*) FROM bot_invocation_actions`) != 4 {
				t.Fatal("multi-source actions duplicated or evidence lost")
			}
			if scalar(t, path, `SELECT count(*) FROM action_evidence e JOIN actions a ON a.id=e.action_id WHERE a.message_id=1 AND e.bot_id=`+strconv.FormatInt(401-removed, 10)) != 2 {
				t.Fatal("valid evidence was not selected for both actions")
			}
		})
	}
}

func TestLateCallerBanUpgradesRecordedIntentOnlyWithNewConfirmedEvidence(t *testing.T) {
	probe := &banProbe{}
	a, path, deletes := botFixture(t, "member", "record", probe.observe)
	enableBan(t, a, -1)
	caller := message(50, "@first_bot @second_bot")
	deliver(t, a, 1, guestReply(caller, 2, 200))
	runModel(t, a, time.Now())
	if err := a.Config.Set(nil, "mode", json.RawMessage(`"enforce"`)); err != nil {
		t.Fatal(err)
	}
	deliver(t, a, 2, guestReply(caller, 3, 201))
	runModel(t, a, time.Now())
	doAction(t, a)
	deliver(t, a, 3, caller)
	drainActions(t, a, path)
	if len(probe.bans()) != 1 || len(deletes.deletes()) != 2 {
		t.Fatal("late caller remained pinned to recorded source")
	}
	if scalar(t, path, `SELECT count(*) FROM bot_action_events e JOIN actions a ON a.id=e.action_id WHERE a.message_id=1 AND e.event='before_upgrade' AND e.status='recorded'`) != 2 {
		t.Fatal("original delete and ban intents missing from audit")
	}
}

func TestEnablingCallerBanPreservesPreviouslyCapturedInvocations(t *testing.T) {
	for _, sourceFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(sourceFirst), func(t *testing.T) {
			probe := &banProbe{}
			a, path, deletes := botFixture(t, "member", "enforce", probe.observe)
			caller := message(50, "@ad_bot")
			if sourceFirst {
				deliver(t, a, 1, guestReply(caller, 2, 200))
				runModel(t, a, time.Now())
				doAction(t, a)
			} else {
				deliver(t, a, 1, caller)
				runModel(t, a, time.Now())
			}
			enableBan(t, a, -1)
			if sourceFirst {
				deliver(t, a, 2, caller)
			} else {
				deliver(t, a, 2, guestReply(caller, 2, 200))
				runModel(t, a, time.Now())
			}
			drainActions(t, a, path)
			if len(probe.bans()) != 0 || scalar(t, path, `SELECT count(*) FROM actions WHERE kind='ban'`) != 0 || len(deletes.deletes()) != 2 {
				t.Fatal("enabling ban retroactively punished archived invocation")
			}
		})
	}
}

func TestCallerBanRevalidatesAfterMembershipNetworkWait(t *testing.T) {
	for _, change := range []string{"blacklist", "policy", "edit"} {
		t.Run(change, func(t *testing.T) {
			var block atomic.Bool
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			probe := &banProbe{}
			a, path := appFixture(t, "member", probe.observe, func(r *http.Request) {
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
			enableBan(t, a, -1)
			if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
				t.Fatal(err)
			}
			caller := message(50, "@ad_bot")
			deliver(t, a, 1, caller)
			deliver(t, a, 2, guestReply(caller, 2, 200))
			doAction(t, a)
			doAction(t, a)
			block.Store(true)
			done := make(chan error, 1)
			go func() { _, err := a.ProcessAction(context.Background()); done <- err }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("membership did not start")
			}
			switch change {
			case "blacklist":
				if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: 200}, false, 9, 9002); err != nil {
					t.Fatal(err)
				}
			case "policy":
				if err := a.Config.Set(nil, "rules.ban.enabled", json.RawMessage(`false`)); err != nil {
					t.Fatal(err)
				}
			case "edit":
				block.Store(false)
				edited := *caller
				edited.Text = "ordinary discussion"
				edited.EditDate = caller.Date + 1
				deliver(t, a, 3, &edited)
			}
			once.Do(func() { close(release) })
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("ban did not finish")
			}
			if len(probe.bans()) != 0 || scalar(t, path, `SELECT count(*) FROM actions WHERE kind='ban' AND status='skipped'`) != 1 {
				t.Fatal("stale ban crossed final dispatch guard")
			}
		})
	}
}

func TestVersionFourMigrationPreservesInvocationHistoryWithoutNewActions(t *testing.T) {
	a, path, _ := botFixture(t, "member", "enforce")
	caller := message(50, "@ad_bot")
	deliver(t, a, 1, caller)
	runModel(t, a, time.Now())
	deliver(t, a, 2, guestReply(caller, 2, 200))
	runModel(t, a, time.Now())
	drainActions(t, a, path)
	ctx := context.Background()
	before, err := a.Store.Revision(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	events := scalar(t, path, `SELECT count(*) FROM bot_action_events`)
	if err := a.Store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`DROP TABLE bot_invocation_actions; PRAGMA user_version=4;`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := s.Revision(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(before.Raw) != string(after.Raw) || scalar(t, path, `SELECT count(*) FROM actions`) != 2 || scalar(t, path, `SELECT count(*) FROM bot_action_events`) != events {
		t.Fatal("migration modified history")
	}
	if scalar(t, path, `SELECT count(*) FROM bot_invocation_actions`) != 1 || scalar(t, path, `SELECT count(*) FROM actions WHERE status='pending' OR kind='ban'`) != 0 {
		t.Fatal("migration lost delete link or created historical ban")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if scalar(t, path, `SELECT count(*) FROM bot_invocation_actions`) != 1 {
		t.Fatal("restart duplicated migration links")
	}
}

func TestCallerBanSelectsLongestEnabledDuration(t *testing.T) {
	for _, duration := range []int{-1, 3600} {
		t.Run(strconv.Itoa(duration), func(t *testing.T) {
			probe := &banProbe{}
			a, path, _ := botFixture(t, "member", "enforce", probe.observe)
			enableBan(t, a, duration)
			c := a.Config.Snapshot()
			short := c.Base.Rules["ban"]
			short.DurationSeconds = 60
			c.Base.Rules["aaa_short"] = short
			saveScoringConfig(t, a, path, c)
			caller := message(50, "@ad_bot")
			deliver(t, a, 1, caller)
			runModel(t, a, time.Now())
			deliver(t, a, 2, guestReply(caller, 2, 200))
			runModel(t, a, time.Now())
			drainActions(t, a, path)
			calls := probe.bans()
			if len(calls) != 1 {
				t.Fatalf("bans=%v", calls)
			}
			if duration == -1 && calls[0].until != 0 {
				t.Fatal("finite rule overrode permanent ban")
			}
			if duration > 0 && calls[0].until < time.Now().Unix()+3590 {
				t.Fatal("shorter rule overrode longer ban")
			}
		})
	}
}

func TestFiniteCallerBanSkipsBasicGroups(t *testing.T) {
	probe := &banProbe{}
	a, path, deletes := botFixture(t, "member", "enforce", probe.observe)
	enableBan(t, a, 3600)
	caller := message(50, "@ad_bot")
	caller.Chat.Type = "group"
	deliver(t, a, 1, caller)
	runModel(t, a, time.Now())
	deliver(t, a, 2, guestReply(caller, 2, 200))
	runModel(t, a, time.Now())
	drainActions(t, a, path)
	if len(probe.bans()) != 0 || len(deletes.deletes()) != 2 {
		t.Fatal("finite ban dispatched to a chat type that ignores until_date")
	}
	if scalar(t, path, `SELECT count(*) FROM actions WHERE kind='ban' AND status='skipped'`) != 1 {
		t.Fatal("unsupported finite ban not audited")
	}
}

func TestShortBanEvidenceCannotAuthorizeAnExistingPermanentAction(t *testing.T) {
	for _, injectLink := range []bool{false, true} {
		t.Run(strconv.FormatBool(injectLink), func(t *testing.T) {
			probe := &banProbe{}
			a, path, _ := botFixture(t, "member", "enforce", probe.observe)
			ctx := context.Background()
			c := a.Config.Snapshot()
			short := c.Base.Rules["ban"]
			short.Enabled = true
			short.DurationSeconds = 3600
			c.Base.Rules["short"] = short
			saveScoringConfig(t, a, path, c)
			for i, id := range []int64{200, 201} {
				if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: id}, true, 9, int64(9001+i)); err != nil {
					t.Fatal(err)
				}
			}
			caller := message(50, "@first_bot @second_bot")
			// The finite-only source is dispatched first and finishes after the
			// permanent-capable source, as can happen with overlapping dispatches.
			deliver(t, a, 1, guestReply(caller, 3, 201))
			finiteSource, err := a.Store.TakeAction(ctx)
			if err != nil || finiteSource == nil {
				t.Fatalf("source claim: %+v %v", finiteSource, err)
			}
			enableBan(t, a, -1)
			deliver(t, a, 2, caller)
			deliver(t, a, 3, guestReply(caller, 2, 200))
			doAction(t, a)
			if _, err := a.Store.CompleteAction(ctx, finiteSource.ID, "succeeded", ""); err != nil {
				t.Fatal(err)
			}
			if injectLink {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`INSERT OR IGNORE INTO bot_invocation_actions(source_action_id,target_action_id) SELECT ?,id FROM actions WHERE kind='ban'`, finiteSource.ID)
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: 200}, false, 9, 9003); err != nil {
				t.Fatal(err)
			}
			drainActions(t, a, path)
			if len(probe.bans()) != 0 || scalar(t, path, `SELECT count(*) FROM actions WHERE kind='ban' AND status='skipped'`) != 1 {
				t.Fatal("finite-only source authorized permanent ban")
			}
			if !injectLink && scalar(t, path, `SELECT count(*) FROM bot_invocation_actions l JOIN actions a ON a.id=l.target_action_id WHERE l.source_action_id=`+strconv.FormatInt(finiteSource.ID, 10)+` AND a.kind='ban'`) != 0 {
				t.Fatal("mismatched rule linked to existing ban")
			}
		})
	}
}
