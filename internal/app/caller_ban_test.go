package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"telegram-antispam-bot/internal/store"
)

type banCall struct{ user, chat, until int64 }
type banProbe struct {
	mu    sync.Mutex
	calls []banCall
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
