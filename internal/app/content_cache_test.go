package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/store"
	"telegram-antispam-bot/internal/telegram"
)

func contentCacheFixture(t *testing.T, mode string, profileOnly bool) (*App, string, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	calls, deletes := &atomic.Int32{}, &atomic.Int32{}
	a, path := appFixture(t, "member", func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "deleteMessage") {
			deletes.Add(1)
		}
	})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Model string `json:"model"`
			State struct {
				Sender struct {
					IsBot bool `json:"is_bot"`
				} `json:"sender"`
			} `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		content, profile := .01, .01
		if request.State.Sender.IsBot {
			if profileOnly {
				profile = .99
			} else {
				content = .99
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": request.Model, "answers": map[string]any{"content.ad": map[string]any{"type": "noul", "noul": content}, "profile.ad": map[string]any{"type": "noul", "noul": profile}}})
	}))
	t.Cleanup(s.Close)
	c := a.Config.Snapshot()
	c.Model.BaseURL = s.URL
	c.Base.Mode = mode
	c.QuestionGroups = map[string]map[string]config.Question{"content": {"ad": {Type: "noul", Instructions: "Only .msg: advertisement?"}}, "profile": {"ad": {Type: "noul", Instructions: "Only .sender: advertisement?"}}}
	c.Base.Questions = []string{"content.ad", "profile.ad"}
	c.Base.Condition = "score >= 100"
	c.Base.Scoring = &config.Scoring{Rules: []config.ScoreRule{{Name: "content", When: "p.content.ad >= 0.83", Add: 100}, {Name: "profile", When: "p.profile.ad >= 0.85", Add: 100}}}
	c.Base.Rules = map[string]config.Rule{"delete": {Enabled: true, Count: 1, Action: "delete"}, "ban": config.Defaults().Base.Rules["ban"]}
	saveScoringConfig(t, a, path, c)
	return a, path, calls, deletes
}

func adMessage(id int, bot int64) *telegram.Message {
	m := message(bot, "same advertisement")
	m.ID = id
	m.From.IsBot = true
	return m
}
func drainModels(t *testing.T, a *App) {
	t.Helper()
	for i := 0; i < 20; i++ {
		worked, err := a.ProcessModel(context.Background(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("model queue did not settle")
}

func TestContentCacheReusesAdvertisingAcrossBotsAndGroupsAfterRestart(t *testing.T) {
	for _, mode := range []string{"record", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			a, path, calls, deletes := contentCacheFixture(t, mode, false)
			deliver(t, a, 1, adMessage(1, 200))
			drainModels(t, a)
			drainActions(t, a, path)
			if err := a.Store.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			a.Store = s
			t.Cleanup(func() { s.Close() })
			second := adMessage(2, 201)
			second.Chat.ID = -200
			second.From.FirstName = "different bot"
			deliver(t, a, 2, second)
			drainModels(t, a)
			drainActions(t, a, path)
			if calls.Load() != 1 {
				t.Fatalf("duplicate ad model calls=%d want 1", calls.Load())
			}
			want := int32(0)
			if mode == "enforce" {
				want = 2
			}
			if deletes.Load() != want {
				t.Fatalf("mode %s deleted=%d", mode, deletes.Load())
			}
			if scalar(t, path, `SELECT count(*) FROM revisions`) != 2 || scalar(t, path, `SELECT count(*) FROM attempts`) != 1 || scalar(t, path, `SELECT count(*) FROM content_cache_matches`) != 1 {
				t.Fatal("cache hit lost raw archive or fabricated inference")
			}
			if scalar(t, path, `SELECT count(*) FROM content_cache_matches WHERE json_extract(evaluation,'$.answers."profile.ad".status')='unknown'`) != 1 {
				t.Fatal("cache reused another sender's profile")
			}
		})
	}
}

func TestProfileOnlyAdvertisingStaysOnModelPath(t *testing.T) {
	a, path, calls, _ := contentCacheFixture(t, "record", true)
	deliver(t, a, 1, adMessage(1, 200))
	drainModels(t, a)
	deliver(t, a, 2, adMessage(2, 201))
	drainModels(t, a)
	if calls.Load() != 2 || scalar(t, path, `SELECT count(*) FROM content_cache`) != 0 {
		t.Fatal("profile-only verdict polluted content cache")
	}
}

func TestContentCacheSeparatesModelAndPolicyVersions(t *testing.T) {
	for _, change := range []string{"model", "question", "threshold"} {
		t.Run(change, func(t *testing.T) {
			a, path, calls, _ := contentCacheFixture(t, "record", false)
			deliver(t, a, 1, adMessage(1, 200))
			drainModels(t, a)
			c := a.Config.Snapshot()
			switch change {
			case "model":
				c.Model.Name = "new-model"
			case "question":
				q := c.QuestionGroups["content"]["ad"]
				q.Instructions += " revised"
				c.QuestionGroups["content"]["ad"] = q
			case "threshold":
				c.Base.Scoring.Rules[0].When = "p.content.ad >= 0.9"
			}
			saveScoringConfig(t, a, path, c)
			deliver(t, a, 2, adMessage(2, 201))
			drainModels(t, a)
			if calls.Load() != 2 {
				t.Fatal("changed classifier reused old cache namespace")
			}
		})
	}
}

func TestContentCacheBypassesModelOutageAndQueueEviction(t *testing.T) {
	a, path, calls, deletes := contentCacheFixture(t, "enforce", false)
	deliver(t, a, 1, adMessage(1, 200))
	drainModels(t, a)
	drainActions(t, a, path)
	if err := a.Config.Set(nil, "queue_limit", json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO meta(key,value) VALUES('model_retry_at',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, time.Now().Add(time.Hour).UnixMilli())
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	waiting := message(50, "different ordinary text")
	waiting.ID = 2
	deliver(t, a, 2, waiting)
	deliver(t, a, 3, adMessage(3, 201))
	drainActions(t, a, path)
	if deletes.Load() != 2 || calls.Load() != 1 {
		t.Fatal("known content waited on model outage")
	}
	if scalar(t, path, `SELECT count(*) FROM revisions WHERE message_id=2 AND status='pending'`) != 1 {
		t.Fatal("cached traffic displaced model queue")
	}
}

func TestModelChangeCancelsQueuedCachedPunishment(t *testing.T) {
	a, path, _, deletes := contentCacheFixture(t, "enforce", false)
	deliver(t, a, 1, adMessage(1, 200))
	drainModels(t, a)
	drainActions(t, a, path)
	deliver(t, a, 2, adMessage(2, 201))
	drainModels(t, a)
	c := a.Config.Snapshot()
	c.Model.Name = "next-model"
	saveScoringConfig(t, a, path, c)
	drainActions(t, a, path)
	if deletes.Load() != 1 || scalar(t, path, `SELECT count(*) FROM actions WHERE message_id=2 AND status='skipped'`) != 1 {
		t.Fatal("old model cache dispatched after model change")
	}
}
