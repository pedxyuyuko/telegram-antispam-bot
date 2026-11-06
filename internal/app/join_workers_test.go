package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"telegram-antispam-bot/internal/telegram"
)

type joinRemote struct {
	profiles, models, kicks          atomic.Int32
	profileFail, modelFail, kickFail atomic.Bool
	role, bio                        atomic.Value
	beforeMembership                 func()
	beforeModel                      func()
	pollRaw                          json.RawMessage
	polls                            atomic.Int32
	subscribed                       atomic.Bool
}

func joinApp(t *testing.T, mode string, enabled bool) (*App, string, *joinRemote) {
	t.Helper()
	a, path := appFixture(t, "member")
	configureJoin(t, a, path, mode, enabled)
	r := &joinRemote{}
	r.role.Store("member")
	r.bio.Store("推广兼职，日入五千")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/v1/systemone") {
			r.models.Add(1)
			if r.beforeModel != nil {
				r.beforeModel()
			}
			if r.modelFail.Load() {
				http.Error(w, "offline", 503)
				return
			}
			var body struct{ Questions map[string]any }
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			answers := map[string]any{}
			for id := range body.Questions {
				answers[id] = map[string]any{"type": "noul", "noul": .99}
			}
			json.NewEncoder(w).Encode(map[string]any{"model": "m", "answers": answers})
			return
		}
		req.ParseMultipartForm(1 << 20)
		if req.MultipartForm != nil {
			defer req.MultipartForm.RemoveAll()
		}
		switch {
		case strings.HasSuffix(req.URL.Path, "/getUpdates"):
			var updates []string
			_ = json.Unmarshal([]byte(req.FormValue("allowed_updates")), &updates)
			for _, name := range updates {
				if name == "chat_member" {
					r.subscribed.Store(true)
				}
			}
			if r.polls.Add(1) == 1 && r.subscribed.Load() && len(r.pollRaw) > 0 {
				fmt.Fprintf(w, `{"ok":true,"result":[%s]}`, r.pollRaw)
			} else {
				select {
				case <-req.Context().Done():
				case <-time.After(20 * time.Millisecond):
					fmt.Fprint(w, `{"ok":true,"result":[]}`)
				}
			}
		case strings.HasSuffix(req.URL.Path, "/getChatMember"):
			user, _ := strconv.ParseInt(req.FormValue("user_id"), 10, 64)
			role := r.role.Load().(string)
			if user == 42 || user == 123 {
				role = "administrator"
			} else if r.beforeMembership != nil {
				r.beforeMembership()
			}
			if role == "unavailable" {
				http.Error(w, "failed", 503)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"status": role, "user": map[string]any{"id": user, "first_name": "member"}, "can_restrict_members": true}})
		case strings.HasSuffix(req.URL.Path, "/getChat"):
			r.profiles.Add(1)
			if r.profileFail.Load() {
				fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"chat not found"}`)
				return
			}
			id, _ := strconv.ParseInt(req.FormValue("chat_id"), 10, 64)
			if id == 99 {
				t.Error("queried inviter biography")
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"id": id, "type": "private", "bio": r.bio.Load().(string)}})
		case strings.HasSuffix(req.URL.Path, "/unbanChatMember"):
			if req.FormValue("user_id") == "99" || req.FormValue("only_if_banned") == "true" {
				t.Errorf("wrong removal: %v", req.Form)
			}
			r.kicks.Add(1)
			fmt.Fprintf(w, `{"ok":true,"result":%t}`, !r.kickFail.Load())
		case strings.HasSuffix(req.URL.Path, "/sendMessage"):
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":100,"chat":{"id":-100,"type":"supergroup"},"date":1}}`)
		default:
			t.Errorf("unexpected Telegram operation: %s", req.URL.Path)
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(s.Close)
	c := a.Config.Snapshot()
	c.Model.BaseURL = s.URL
	c.Model.RetrySeconds = 1
	raw, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Config.Reload(); err != nil {
		t.Fatal(err)
	}
	tgcfg := c.Telegram
	tgcfg.APIBase = s.URL
	tg, err := telegram.New(tgcfg, "123:offline", s.Client(), 0, a.Log)
	if err != nil {
		t.Fatal(err)
	}
	tg.Me = a.Telegram.Me
	a.Telegram = tg
	return a, path, r
}

func memberUpdate(id, chat, user int64, old, next string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"update_id":%d,"chat_member":{"chat":{"id":%d,"type":"supergroup"},"from":{"id":99,"first_name":"Inviter"},"date":%d,"old_chat_member":{"status":%q,"user":{"id":%d,"first_name":"Member"}},"new_chat_member":{"status":%q,"user":{"id":%d,"first_name":"New","last_name":"Member"}},"unknown":"retained"}}`, id, chat, user+1700000000+id, old, user, next, user))
}

func deliverMembership(t *testing.T, a *App, id, chat, user int64, old, next string) {
	t.Helper()
	if err := a.Store.Capture(context.Background(), []json.RawMessage{memberUpdate(id, chat, user, old, next)}); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.ProcessInbox(context.Background()); err != nil || !ok {
		t.Fatalf("membership inbox=%v %v", ok, err)
	}
}

func runJoin(t *testing.T, a *App, now time.Time) {
	t.Helper()
	if ok, err := a.ProcessJoinModel(context.Background(), now); err != nil || !ok {
		t.Fatalf("join worker=%v %v", ok, err)
	}
}
func runJoinAction(t *testing.T, a *App) {
	t.Helper()
	if _, err := a.ProcessJoinAction(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestJoinDetectionUsesMemberContextAndIndependentModes(t *testing.T) {
	a, path, r := joinApp(t, "enforce", true)
	chat := int64(-200)
	if err := a.Config.SetJoin(&chat, "mode", json.RawMessage(`"record"`)); err != nil {
		t.Fatal(err)
	}
	for i, chat := range []int64{-100, -200} {
		deliverMembership(t, a, int64(i+1), chat, 50, "left", "member")
		runJoin(t, a, time.Now())
		runJoinAction(t, a)
	}
	if r.models.Load() != 2 || r.profiles.Load() != 2 || r.kicks.Load() != 1 {
		t.Fatalf("models=%d profiles=%d removals=%d", r.models.Load(), r.profiles.Load(), r.kicks.Load())
	}
	if scalar(t, path, `SELECT count(*) FROM join_attempts WHERE json_extract(request,'$.state.member.name')='New Member' AND json_extract(request,'$.state.member.bio')='推广兼职，日入五千' AND json_type(request,'$.state.message') IS NULL AND json_extract(evaluation,'$.score')=100`) != 2 {
		t.Fatal("wrong joining-member material or score")
	}
	if scalar(t, path, `SELECT count(*) FROM join_actions WHERE status='recorded'`) != 1 || scalar(t, path, `SELECT count(*) FROM join_actions WHERE status='succeeded'`) != 1 {
		t.Fatal("join modes were mixed")
	}
	if scalar(t, path, `SELECT count(*) FROM attempts`) != 0 || scalar(t, path, `SELECT count(*) FROM hits`) != 0 {
		t.Fatal("join detection leaked into message records")
	}
	if scalar(t, path, `SELECT count(*) FROM join_events WHERE json_extract(raw,'$.chat_member.unknown')='retained' AND profile<>''`) != 2 {
		t.Fatal("raw event/profile audit lost")
	}
}

func TestJoinDisabledProtectedAndServiceEventsDoNotRunInference(t *testing.T) {
	a, path, r := joinApp(t, "enforce", false)
	deliverMembership(t, a, 1, -100, 50, "left", "member")
	chat := int64(-100)
	if err := a.Config.SetJoin(&chat, "enabled", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	for i, id := range []int64{9, 123} {
		deliverMembership(t, a, int64(i+2), chat, id, "left", "member")
	}
	deliverMembership(t, a, 4, chat, 51, "left", "administrator")
	deliverMembership(t, a, 5, chat, 52, "member", "administrator")
	deliverRawMessage(t, a, 6, `{"message_id":6,"chat":{"id":-100,"type":"supergroup"},"from":{"id":99,"first_name":"Inviter"},"new_chat_members":[{"id":50,"first_name":"New"}],"unknown":"service-preserved"}`)
	if ok, err := a.ProcessJoinModel(context.Background(), time.Now()); err != nil || ok {
		t.Fatal("protected or disabled arrival was queued")
	}
	if ok, err := a.ProcessModel(context.Background(), time.Now()); err != nil || ok {
		t.Fatal("join service message entered message inference")
	}
	if r.models.Load() != 0 || r.kicks.Load() != 0 {
		t.Fatal("protected arrival triggered work")
	}
	if scalar(t, path, `SELECT count(*) FROM revisions WHERE status='ignored' AND json_extract(raw,'$.unknown')='service-preserved'`) != 1 {
		t.Fatal("service message not preserved")
	}
}

func TestJoinMissingBiographyStillEvaluatesNameAndRetryFreezesContext(t *testing.T) {
	a, path, r := joinApp(t, "record", true)
	r.profileFail.Store(true)
	r.modelFail.Store(true)
	deliverMembership(t, a, 1, -100, 50, "left", "member")
	runJoin(t, a, time.Now())
	r.profileFail.Store(false)
	r.bio.Store("changed biography")
	r.modelFail.Store(false)
	runJoin(t, a, time.Now().Add(time.Minute))
	if r.profiles.Load() != 1 || r.models.Load() != 2 {
		t.Fatal("model retry refreshed frozen biography")
	}
	if scalar(t, path, `SELECT count(*) FROM join_attempts WHERE json_extract(request,'$.state.member.name')='New Member' AND json_type(request,'$.state.member.bio')='null' AND json_extract(request,'$.state.member.bio_status')='unavailable'`) != 2 {
		t.Fatal("missing biography or frozen request lost")
	}
	if scalar(t, path, `SELECT count(*) FROM join_events WHERE json_extract(profile,'$.error')<>''`) != 1 {
		t.Fatal("lookup failure not archived")
	}
}

func TestJoinOffDuringModelResponseCancelsBeforeReenable(t *testing.T) {
	a, path, r := joinApp(t, "enforce", true)
	r.beforeModel = func() {
		if _, err := a.joinCommand(context.Background(), message(42, ""), "off"); err != nil {
			t.Error(err)
		}
		if _, err := a.joinCommand(context.Background(), message(42, ""), "on"); err != nil {
			t.Error(err)
		}
	}
	deliverMembership(t, a, 1, -100, 50, "left", "member")
	runJoin(t, a, time.Now())
	runJoinAction(t, a)
	if r.kicks.Load() != 0 || scalar(t, path, `SELECT count(*) FROM join_attempts`) != 1 || scalar(t, path, `SELECT count(*) FROM join_events WHERE status='cancelled'`) != 1 {
		t.Fatal("off/on allowed an old in-flight decision")
	}
}

func TestJoinRemovalRechecksMembershipPolicyAndPendingEvents(t *testing.T) {
	for _, change := range []string{"administrator", "creator", "left", "kicked", "unavailable", "policy", "off_on", "record_enforce", "pending_departure"} {
		t.Run(change, func(t *testing.T) {
			a, path, r := joinApp(t, "enforce", true)
			deliverMembership(t, a, 1, -100, 50, "left", "member")
			runJoin(t, a, time.Now())
			var armed atomic.Bool
			armed.Store(true)
			r.beforeMembership = func() {
				if !armed.Swap(false) {
					return
				}
				chat := int64(-100)
				switch change {
				case "policy":
					if err := a.Config.SetJoin(&chat, "condition", json.RawMessage(`"score >= 200"`)); err != nil {
						t.Error(err)
					}
				case "off_on":
					for _, cmd := range []string{"off", "on"} {
						if _, err := a.joinCommand(context.Background(), message(42, ""), cmd); err != nil {
							t.Error(err)
						}
					}
				case "record_enforce":
					for _, cmd := range []string{"mode record", "mode enforce"} {
						if _, err := a.joinCommand(context.Background(), message(42, ""), cmd); err != nil {
							t.Error(err)
						}
					}
				case "pending_departure":
					if err := a.Store.Capture(context.Background(), []json.RawMessage{memberUpdate(2, -100, 50, "member", "left")}); err != nil {
						t.Error(err)
					}
				}
			}
			switch change {
			case "administrator", "creator", "left", "kicked", "unavailable":
				r.role.Store(change)
			}
			runJoinAction(t, a)
			if r.kicks.Load() != 0 || scalar(t, path, `SELECT count(*) FROM join_actions WHERE status='skipped'`) != 1 {
				t.Fatalf("unsafe removal after %s", change)
			}
		})
	}
}

func TestJoinFailedRemovalIsArchivedWithoutReplay(t *testing.T) {
	a, path, r := joinApp(t, "enforce", true)
	r.kickFail.Store(true)
	deliverMembership(t, a, 1, -100, 50, "left", "member")
	runJoin(t, a, time.Now())
	runJoinAction(t, a)
	runJoinAction(t, a)
	if r.kicks.Load() != 1 || scalar(t, path, `SELECT count(*) FROM join_actions WHERE status='failed' AND detail<>''`) != 1 {
		t.Fatal("failed removal evidence lost or dispatched twice")
	}
}

func TestJoinRuntimeSubscribesAndDrainsDurableMembershipWork(t *testing.T) {
	a, path, r := joinApp(t, "enforce", true)
	r.pollRaw = memberUpdate(1, -100, 50, "left", "member")
	cfg := a.Config.Snapshot().Telegram
	cfg.APIBase = a.Config.Snapshot().Model.BaseURL
	durable := &telegram.DurableClient{HTTP: &http.Client{Timeout: 2 * time.Second}, Capture: a.Store.Capture}
	tg, err := telegram.New(cfg, "123:offline", durable, 0, a.Log)
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
			if err := db.QueryRow(`SELECT count(*) FROM join_actions WHERE status='succeeded'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n == 1 {
				finished = true
			}
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !r.subscribed.Load() || r.models.Load() != 1 || r.kicks.Load() != 1 {
		t.Fatalf("runtime join work not connected: subscribed=%v models=%d kicks=%d", r.subscribed.Load(), r.models.Load(), r.kicks.Load())
	}
	if scalar(t, path, `SELECT count(*) FROM updates WHERE processed=1`) != 1 {
		t.Fatal("polling did not durably capture membership")
	}
}
