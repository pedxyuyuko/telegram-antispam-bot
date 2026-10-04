package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"telegram-antispam-bot/internal/telegram"
)

func modelFixture(t *testing.T, a *App, path string, offline *atomic.Bool) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline != nil && offline.Load() {
			w.WriteHeader(503)
			w.Write([]byte("offline"))
			return
		}
		w.Write([]byte(`{"model":"m","answers":{"promotion":{"type":"noul","noul":0.99},"fraud":{"type":"noul","noul":0.99},"illicit":{"type":"noul","noul":0.99},"benign":{"type":"noul","noul":0.01},"violation":{"type":"noul","noul":0.99}}}`))
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
}
func scalar(t *testing.T, path, query string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func runModel(t *testing.T, a *App, now time.Time) {
	t.Helper()
	if worked, err := a.ProcessModel(context.Background(), now); err != nil || !worked {
		t.Fatalf("model: %v %v", worked, err)
	}
}

func TestRecordModeArchivesInferenceAndSuppressesAllPunishments(t *testing.T) {
	var operations atomic.Int32
	a, path := appFixture(t, "member", func(r *http.Request) {
		if strings.Contains(r.URL.Path, "deleteMessage") || strings.Contains(r.URL.Path, "restrictChatMember") || strings.Contains(r.URL.Path, "unbanChatMember") {
			operations.Add(1)
		}
	})
	modelFixture(t, a, path, nil)
	deliver(t, a, 1, message(50, "sample"))
	runModel(t, a, time.Now())
	if worked, err := a.ProcessAction(context.Background()); err != nil || worked {
		t.Fatalf("record mode dispatched action: %v %v", worked, err)
	}
	if operations.Load() != 0 {
		t.Fatal("record mode called Telegram moderation")
	}
	if scalar(t, path, `SELECT count(*) FROM attempts WHERE error='' AND json_extract(probabilities,'$.violation')=0.99 AND json_extract(request,'$.state.sender_profile.bio')='profile'`) != 1 {
		t.Fatal("model evidence missing")
	}
}

func TestEnforcementDispatchesConfiguredDeleteAndMute(t *testing.T) {
	var operations atomic.Int32
	a, path := appFixture(t, "member", func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "deleteMessage") || strings.HasSuffix(r.URL.Path, "restrictChatMember") {
			operations.Add(1)
		}
	})
	modelFixture(t, a, path, nil)
	if err := a.Config.Set(nil, "mode", json.RawMessage(`"enforce"`)); err != nil {
		t.Fatal(err)
	}
	if err := a.Config.Set(nil, "rules.repeat.count", json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	deliver(t, a, 1, message(50, "sample"))
	runModel(t, a, time.Now())
	for range 2 {
		if worked, err := a.ProcessAction(context.Background()); err != nil || !worked {
			t.Fatalf("dispatch: %v %v", worked, err)
		}
	}
	if operations.Load() != 2 || scalar(t, path, `SELECT count(*) FROM actions WHERE status='succeeded'`) != 2 {
		t.Fatal("configured penalties were not dispatched")
	}
}

func TestModeChangeAndAdministratorProtectionCancelPendingActions(t *testing.T) {
	for _, modeChange := range []bool{false, true} {
		a, path := appFixture(t, "administrator")
		modelFixture(t, a, path, nil)
		if err := a.Config.Set(nil, "mode", json.RawMessage(`"enforce"`)); err != nil {
			t.Fatal(err)
		}
		deliver(t, a, 1, message(50, "sample"))
		runModel(t, a, time.Now())
		if modeChange {
			if err := a.Config.Set(nil, "mode", json.RawMessage(`"record"`)); err != nil {
				t.Fatal(err)
			}
		}
		if worked, err := a.ProcessAction(context.Background()); err != nil || !worked {
			t.Fatalf("dispatch: %v %v", worked, err)
		}
		if scalar(t, path, `SELECT count(*) FROM actions WHERE status='skipped'`) != 1 {
			t.Fatal("unsafe pending action executed")
		}
	}
}

func TestOutageRetainsNewestMessagesAndResumesWithEvidence(t *testing.T) {
	a, path := appFixture(t, "member")
	var offline atomic.Bool
	offline.Store(true)
	modelFixture(t, a, path, &offline)
	if err := a.Config.Set(nil, "queue_limit", json.RawMessage(`2`)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		m := message(50, "sample")
		m.ID = i
		deliver(t, a, int64(i), m)
	}
	now := time.Now()
	runModel(t, a, now)
	m := message(50, "newest")
	m.ID = 3
	deliver(t, a, 3, m)
	if worked, err := a.ProcessModel(context.Background(), now); err != nil || worked {
		t.Fatal("failure backoff ignored")
	}
	offline.Store(false)
	runModel(t, a, now.Add(2*time.Second))
	runModel(t, a, now.Add(2*time.Second))
	stats, err := a.Store.Stats(context.Background(), -100)
	if err != nil {
		t.Fatal(err)
	}
	if stats["evicted"] != 1 || stats["succeeded"] != 2 {
		t.Fatalf("outage queue: %v", stats)
	}
	if scalar(t, path, `SELECT count(*) FROM attempts WHERE error<>''`) != 1 {
		t.Fatal("failure evidence missing")
	}
}

func TestSenderChatWithTelegramFakeBotSenderIsStillInspected(t *testing.T) {
	a, _ := appFixture(t, "member")
	m := message(1087968824, "channel promotion")
	m.From.IsBot = true
	m.SenderChat = &telegram.Chat{ID: -200, Type: "channel"}
	deliver(t, a, 1, m)
	stats, err := a.Store.Stats(context.Background(), -100)
	if err != nil {
		t.Fatal(err)
	}
	if stats["pending"] != 1 {
		t.Fatal("channel content was mistaken for bot-generated traffic")
	}
}

func TestRunStopsAllLoopsOnCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	a, _ := appFixture(t, "member", func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "getUpdates") {
			select {
			case started <- struct{}{}:
			default:
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("poller did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown stuck")
	}
}

func TestKickPreservesExistingBanAndAbsentMembership(t *testing.T) {
	for _, membership := range []string{"kicked", "left"} {
		t.Run(membership, func(t *testing.T) {
			var removals atomic.Int32
			a, path := appFixture(t, membership, func(r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "unbanChatMember") {
					removals.Add(1)
				}
			})
			modelFixture(t, a, path, nil)
			for key, value := range map[string]string{
				"mode": `"enforce"`, "rules.delete.enabled": `false`,
				"rules.repeat": `{"enabled":true,"question":"violation","threshold":0.9,"count":1,"window_seconds":0,"action":"kick","duration_seconds":0}`,
			} {
				if err := a.Config.Set(nil, key, json.RawMessage(value)); err != nil {
					t.Fatal(err)
				}
			}
			deliver(t, a, 1, message(50, "sample"))
			runModel(t, a, time.Now())
			if worked, err := a.ProcessAction(context.Background()); err != nil || !worked {
				t.Fatalf("dispatch: %v %v", worked, err)
			}
			if removals.Load() != 0 {
				t.Fatal("kick altered existing ban or absent membership")
			}
			if scalar(t, path, `SELECT count(*) FROM actions WHERE status='skipped'`) != 1 {
				t.Fatal("skipped action was not audited")
			}
		})
	}
}
