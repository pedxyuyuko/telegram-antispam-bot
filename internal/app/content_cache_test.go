package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
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

func TestCachedAdUsesThisInvocationsCallerAndLearnsNewBot(t *testing.T) {
	for _, mode := range []string{"record", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			a, path, calls, _ := contentCacheFixture(t, mode, false)
			enableBan(t, a, -1)
			for i := 0; i < 2; i++ {
				caller := message(int64(50+i), "call bot")
				caller.ID = i*2 + 1
				deliver(t, a, int64(caller.ID), caller)
				drainModels(t, a)
				ad := guestReply(caller, i*2+2, int64(200+i))
				ad.Text = "same advertisement"
				deliver(t, a, int64(ad.ID), ad)
				drainModels(t, a)
				drainActions(t, a, path)
			}
			if calls.Load() != 3 || scalar(t, path, `SELECT count(*) FROM content_cache_matches`) != 1 {
				t.Fatal("duplicate guest ad missed cache")
			}
			status := "recorded"
			if mode == "enforce" {
				status = "succeeded"
			}
			if scalar(t, path, `SELECT count(*) FROM actions WHERE kind='ban' AND status='`+status+`' AND ((message_id=1 AND user_id=50) OR (message_id=3 AND user_id=51))`) != 2 {
				t.Fatal("cached ad lost current caller or reused source caller")
			}
			entry, err := a.Store.BotBlacklist(context.Background(), 201)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "enforce" && (entry == nil || !entry.Active) {
				t.Fatal("cached successful deletion did not learn new bot")
			}
			if mode == "record" && entry != nil {
				t.Fatal("record cache match learned blacklist")
			}
		})
	}
}

func TestCachedInvocationActionsRecheckSourceModel(t *testing.T) {
	a, path, _, _ := contentCacheFixture(t, "enforce", false)
	enableBan(t, a, -1)
	deliver(t, a, 1, adMessage(1, 200))
	drainModels(t, a)
	drainActions(t, a, path)
	caller := message(50, "call bot")
	caller.ID = 2
	deliver(t, a, 2, caller)
	drainModels(t, a)
	ad := guestReply(caller, 3, 201)
	ad.Text = "same advertisement"
	deliver(t, a, 3, ad)
	doAction(t, a)
	c := a.Config.Snapshot()
	c.Model.Name = "new-model"
	saveScoringConfig(t, a, path, c)
	drainActions(t, a, path)
	if scalar(t, path, `SELECT count(*) FROM actions WHERE message_id=2 AND status='skipped'`) != 2 {
		t.Fatal("cached invocation source survived model change")
	}
}

func TestCachedActionVersionAndRoleProtectionsRemainActive(t *testing.T) {
	for _, change := range []string{"edit", "owner", "record"} {
		t.Run(change, func(t *testing.T) {
			a, path, _, deletes := contentCacheFixture(t, "enforce", false)
			deliver(t, a, 1, adMessage(1, 200))
			drainModels(t, a)
			drainActions(t, a, path)
			target := adMessage(2, 201)
			deliver(t, a, 2, target)
			switch change {
			case "edit":
				target.Text = "ordinary discussion"
				target.EditDate = target.Date + 1
				deliver(t, a, 3, target)
			case "owner":
				c := a.Config.Snapshot()
				c.OwnerIDs = append(c.OwnerIDs, 201)
				saveScoringConfig(t, a, path, c)
			case "record":
				if err := a.Config.Set(nil, "mode", json.RawMessage(`"record"`)); err != nil {
					t.Fatal(err)
				}
			}
			drainActions(t, a, path)
			if deletes.Load() != 1 {
				t.Fatal("cached action bypassed current protection")
			}
		})
	}
}

func TestCacheNamespaceIsRecheckedAfterMembershipWait(t *testing.T) {
	a, path, _, deletes := contentCacheFixture(t, "enforce", false)
	deliver(t, a, 1, adMessage(1, 200))
	drainModels(t, a)
	drainActions(t, a, path)
	deliver(t, a, 2, adMessage(2, 201))
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "getChatMember") {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			w.Write([]byte(`{"ok":true,"result":{"status":"member","user":{"id":201,"first_name":"bot"}}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "deleteMessage") {
			deletes.Add(1)
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	t.Cleanup(s.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	cfg := a.Config.Snapshot().Telegram
	cfg.APIBase = s.URL
	tg, err := telegram.New(cfg, "123:offline", s.Client(), 0, a.Log)
	if err != nil {
		t.Fatal(err)
	}
	tg.Me = a.Telegram.Me
	a.Telegram = tg
	done := make(chan error, 1)
	go func() { _, err := a.ProcessAction(context.Background()); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("membership did not start")
	}
	c := a.Config.Snapshot()
	c.Model.Name = "new-model"
	saveScoringConfig(t, a, path, c)
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("action did not finish")
	}
	if deletes.Load() != 1 {
		t.Fatal("stale cache crossed final dispatch check")
	}
}

func TestVersionFiveMigrationPreservesHistoryAndStartsWithEmptyCache(t *testing.T) {
	a, path, _, _ := contentCacheFixture(t, "record", false)
	deliver(t, a, 1, adMessage(1, 200))
	drainModels(t, a)
	before, err := a.Store.Revision(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`DROP TABLE content_cache_matches; DROP TABLE content_cache; PRAGMA user_version=5;`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := s.Revision(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(before.Raw) != string(after.Raw) || scalar(t, path, `SELECT count(*) FROM attempts`) != 1 || scalar(t, path, `SELECT count(*) FROM actions`) != 1 || scalar(t, path, `SELECT count(*) FROM content_cache`) != 0 {
		t.Fatal("migration changed existing evidence or created retrospective cache entries")
	}
}

func TestFailedAndStaleModelResultsCannotPublishContentCache(t *testing.T) {
	for _, scenario := range []string{"failed", "edited", "evicted"} {
		t.Run(scenario, func(t *testing.T) {
			a, path, _, _ := contentCacheFixture(t, "record", false)
			deliver(t, a, 1, adMessage(1, 200))
			j, err := a.Store.Claim(context.Background(), time.Now())
			if err != nil || j == nil {
				t.Fatalf("claim=%+v %v", j, err)
			}
			var failure error
			switch scenario {
			case "failed":
				failure = errors.New("model unavailable")
			case "edited":
				m := adMessage(1, 200)
				m.Text = "newer version"
				m.EditDate = m.Date + 1
				deliver(t, a, 2, m)
			case "evicted":
				if err := a.Config.Set(nil, "queue_limit", json.RawMessage(`1`)); err != nil {
					t.Fatal(err)
				}
				m := message(50, "ordinary")
				m.ID = 2
				deliver(t, a, 2, m)
			}
			model := a.Config.Snapshot().Model
			result := decision.Result{ModelID: model.Name, ModelEndpoint: model.BaseURL, Probabilities: map[string]float64{"content.ad": .99, "profile.ad": .01}}
			if _, err := a.Store.Finish(context.Background(), *j, result, failure, time.Now(), 10); err != nil {
				t.Fatal(err)
			}
			if scalar(t, path, `SELECT count(*) FROM content_cache`) != 0 || scalar(t, path, `SELECT count(*) FROM attempts`) != 1 {
				t.Fatal("failed/stale result entered cache or attempt was lost")
			}
		})
	}
}
