package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"telegram-antispam-bot/internal/telegram"
)

func referenceGateway(t *testing.T, a *App, bio *atomic.Value) *atomic.Int32 {
	t.Helper()
	calls := &atomic.Int32{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1 << 20)
		switch {
		case strings.HasSuffix(r.URL.Path, "getChatMember"):
			fmt.Fprint(w, `{"ok":true,"result":{"status":"member","user":{"id":50,"first_name":"Member"}}}`)
		case strings.HasSuffix(r.URL.Path, "getChat"):
			query := r.FormValue("chat_id")
			if strings.HasPrefix(query, "@") {
				calls.Add(1)
				if query == "@missingbot" {
					fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"chat not found"}`)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"id": -200, "type": "channel", "username": strings.TrimPrefix(query, "@"), "title": "Referenced channel", "description": bio.Load().(string)}})
				return
			}
			fmt.Fprint(w, `{"ok":true,"result":{"id":50,"type":"private","bio":"sender profile"}}`)
		default:
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(s.Close)
	cfg := a.Config.Snapshot().Telegram
	cfg.APIBase = s.URL
	client, err := telegram.New(cfg, "123:offline", s.Client(), 0, a.Log)
	if err != nil {
		t.Fatal(err)
	}
	client.Me = a.Telegram.Me
	a.Telegram = client
	return calls
}

func TestNewMessageContextAttachesReferencedChatProfileAndAudit(t *testing.T) {
	a, path := appFixture(t, "member")
	modelFixture(t, a, path, nil)
	var bio atomic.Value
	bio.Store("channel description")
	calls := referenceGateway(t, a, &bio)
	for i := 1; i <= 2; i++ {
		m := message(50, "hi @examplechannel")
		m.ID = i
		deliver(t, a, int64(i), m)
		runModel(t, a, time.Now())
	}
	if calls.Load() != 1 {
		t.Fatalf("reference API calls=%d", calls.Load())
	}
	if scalar(t, path, `SELECT count(*) FROM revisions WHERE json_extract(state,'$.message[1].profile.name')='Referenced channel' AND json_extract(state,'$.message[1].profile.bio')='channel description' AND json_extract(state,'$.sender.bio')='sender profile' AND json_type(state,'$.msg') IS NULL`) != 2 {
		t.Fatal("segmented profile material missing")
	}
	if scalar(t, path, `SELECT count(*) FROM reference_profiles WHERE status='available'`) != 1 || scalar(t, path, `SELECT count(*) FROM revision_reference_profiles`) != 2 {
		t.Fatal("reference snapshot linkage lost")
	}
}

func TestReferenceFailureAndInviteRemainExplicitWithoutBlockingInference(t *testing.T) {
	a, path := appFixture(t, "member")
	modelFixture(t, a, path, nil)
	var bio atomic.Value
	bio.Store("description")
	calls := referenceGateway(t, a, &bio)
	deliver(t, a, 1, message(50, "@missingbot https://t.me/+PrivateInvite"))
	runModel(t, a, time.Now())
	if calls.Load() != 1 || scalar(t, path, `SELECT count(*) FROM reference_profiles WHERE status IN ('unavailable','unsupported')`) != 2 {
		t.Fatal("failed reference lookup was lost or invite was queried")
	}
	if scalar(t, path, `SELECT count(*) FROM attempts WHERE error=''`) != 1 || scalar(t, path, `SELECT count(*) FROM meta WHERE key='model_retry_at'`) != 0 {
		t.Fatal("profile failure paused model inference")
	}
}

func TestReferenceMaterialIsFrozenAcrossModelRetry(t *testing.T) {
	a, path := appFixture(t, "member")
	var offline atomic.Bool
	offline.Store(true)
	modelFixture(t, a, path, &offline)
	var bio atomic.Value
	bio.Store("first profile")
	calls := referenceGateway(t, a, &bio)
	deliver(t, a, 1, message(50, "@examplechannel"))
	runModel(t, a, time.Now())
	bio.Store("changed profile")
	offline.Store(false)
	runModel(t, a, time.Now().Add(time.Minute))
	if calls.Load() != 1 || scalar(t, path, `SELECT count(*) FROM attempts WHERE json_extract(request,'$.state.message[0].profile.bio')='first profile'`) != 2 {
		t.Fatal("retry changed frozen reference snapshot")
	}
	if scalar(t, path, `SELECT count(*) FROM reference_profiles`) != 1 {
		t.Fatal("retry queried profiles again")
	}
}

func TestLegacyPendingMessageKeepsItsOriginalContextLayout(t *testing.T) {
	a, path := appFixture(t, "member")
	modelFixture(t, a, path, nil)
	deliver(t, a, 1, message(50, "old queued text"))
	j, err := a.Store.Revision(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	// A new delivery must carry a format version, so pre-upgrade jobs can retain
	// their old .msg questions and material layout.
	if j.Snapshot.ContextVersion != telegram.SegmentedContextVersion {
		t.Fatal("new job lacks context format version")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE revisions SET snapshot=json_remove(snapshot,'$.context_version') WHERE id=1`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	runModel(t, a, time.Now())
	if scalar(t, path, `SELECT count(*) FROM attempts WHERE json_extract(request,'$.state.msg.text')='old queued text' AND json_type(request,'$.state.message') IS NULL`) != 1 {
		t.Fatal("old pending job changed context layout")
	}
}

func TestChangedReferenceProfileInvalidatesContentReuse(t *testing.T) {
	a, path, models, _ := contentCacheFixture(t, "record", false)
	var bio atomic.Value
	bio.Store("first description")
	lookups := referenceGateway(t, a, &bio)
	first := adMessage(1, 200)
	first.Text += " @examplechannel"
	deliver(t, a, 1, first)
	drainModels(t, a)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-time.Hour).UnixMilli()
	_, err = db.Exec(`UPDATE reference_profiles SET fetched_at=?,payload=json_set(payload,'$.fetched_at',?)`, expired, expired)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	bio.Store("changed description")
	for i := 2; i <= 3; i++ {
		m := adMessage(i, int64(199+i))
		m.Text += " @examplechannel"
		deliver(t, a, int64(i), m)
		drainModels(t, a)
	}
	if lookups.Load() != 2 || models.Load() != 2 || scalar(t, path, `SELECT count(*) FROM content_cache_matches`) != 1 {
		t.Fatalf("lookups=%d model calls=%d", lookups.Load(), models.Load())
	}
	if scalar(t, path, `SELECT count(*) FROM revisions WHERE json_extract(state,'$.message[1].profile.bio')='changed description'`) != 2 {
		t.Fatal("cached judgment bypassed refreshed reference snapshot")
	}
}

func TestUnavailableMentionProfilePreventsContentCachePublication(t *testing.T) {
	a, path, models, _ := contentCacheFixture(t, "record", false)
	var bio atomic.Value
	bio.Store("unused")
	referenceGateway(t, a, &bio)
	for i := 1; i <= 2; i++ {
		m := adMessage(i, int64(199+i))
		m.Text += " @missingbot"
		deliver(t, a, int64(i), m)
		drainModels(t, a)
	}
	if models.Load() != 2 || scalar(t, path, `SELECT count(*) FROM content_cache`) != 0 {
		t.Fatal("unavailable target profile became a reusable content decision")
	}
}

func TestReferenceExpansionStopsAtTheRequestedChatProfile(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int32
	}{{"https://t.me/examplechannel", 1}, {"@examplechannel", 2}} {
		t.Run(tc.text, func(t *testing.T) {
			a, path := appFixture(t, "member")
			modelFixture(t, a, path, nil)
			var bio atomic.Value
			bio.Store("more https://t.me/childchannel")
			calls := referenceGateway(t, a, &bio)
			deliver(t, a, 1, message(50, tc.text))
			runModel(t, a, time.Now())
			if calls.Load() != tc.want {
				t.Fatalf("profile expansion calls=%d want %d", calls.Load(), tc.want)
			}
		})
	}
}

func TestExhaustedReferenceBudgetStaysLocalToMessage(t *testing.T) {
	a, _ := appFixture(t, "member")
	var bio atomic.Value
	bio.Store("description")
	calls := referenceGateway(t, a, &bio)
	budget, cancel := context.WithCancel(context.Background())
	cancel()
	ref, _ := telegram.ParseReference("@examplechannel")
	requests := 0
	result := a.lookupReferenceWithinBudget(budget, ref, 0, &requests)
	if result.Status != "limit" || requests != 0 || calls.Load() != 0 {
		t.Fatalf("expired budget became shared query failure: %+v requests=%d calls=%d", result, requests, calls.Load())
	}
}
