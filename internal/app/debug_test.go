package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/telegram"
)

func captureDebug(a *App) *bytes.Buffer {
	b := &bytes.Buffer{}
	a.Log = slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return b
}

func events(t *testing.T, b *bytes.Buffer, name string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["event"] == name {
			found = append(found, row)
		}
	}
	return found
}

func TestDebugTraceExplainsRecordModeWithoutLoggingPayloads(t *testing.T) {
	a, path := appFixture(t, "member")
	log := captureDebug(a)
	if err := a.Config.Set(nil, "debug", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	groupedModel(t, a, path, "record")
	deliver(t, a, 1, message(50, "PRIVATE_MESSAGE_SENTINEL"))
	runModel(t, a, time.Now())
	for _, name := range []string{"message.received", "message.archived", "profile.started", "profile.completed", "context.ready", "model.started", "model.completed", "model.answers", "policy.scored", "policy.rules", "action.planned", "model.archived"} {
		rows := events(t, log, name)
		if len(rows) != 1 {
			t.Fatalf("event %s count=%d\n%s", name, len(rows), log.String())
		}
		r := rows[0]
		if r["level"] != "DEBUG" || r["chat"] != float64(-100) || r["message"] != float64(1) || r["update"] != float64(1) {
			t.Fatalf("trace lost identity: %v", r)
		}
		if name != "message.received" && r["revision"] != float64(1) {
			t.Fatalf("trace lost revision: %v", r)
		}
	}
	scored := events(t, log, "policy.scored")[0]
	if scored["score"] != float64(60) || scored["condition_result"] != "true" {
		t.Fatalf("score missing: %v", scored)
	}
	planned := events(t, log, "action.planned")[0]
	if planned["status"] != "recorded" || planned["action"] != "delete" {
		t.Fatalf("record mode misreported: %v", planned)
	}
	if strings.Contains(log.String(), "PRIVATE_MESSAGE_SENTINEL") || strings.Contains(log.String(), "123:secret") || strings.Contains(log.String(), `"bio"`) {
		t.Fatal("sensitive payload leaked to diagnostic logs")
	}
}

func TestDebugFlagIsPerGroupAndChangesWithoutRestart(t *testing.T) {
	a, path := appFixture(t, "member")
	log := captureDebug(a)
	modelFixture(t, a, path, nil)
	if err := a.Config.Set(nil, "debug", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	quiet := int64(-200)
	if err := a.Config.Set(&quiet, "debug", json.RawMessage(`false`)); err != nil {
		t.Fatal(err)
	}
	m := message(50, "quiet message")
	m.Chat.ID = quiet
	deliver(t, a, 1, m)
	runModel(t, a, time.Now())
	if len(events(t, log, "model.started")) != 0 || strings.Contains(log.String(), `"level":"DEBUG"`) {
		t.Fatal("group false override leaked debug logs")
	}
	deliver(t, a, 2, message(50, "visible message"))
	runModel(t, a, time.Now())
	if len(events(t, log, "model.started")) != 1 {
		t.Fatal("shared debug flag had no effect")
	}
	chat := int64(-100)
	if err := a.Config.Set(&chat, "debug", json.RawMessage(`false`)); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	deliver(t, a, 3, message(50, "quiet again"))
	runModel(t, a, time.Now())
	if strings.Contains(log.String(), `"level":"DEBUG"`) {
		t.Fatal("debug disable required a restart")
	}
}

func TestDebugTraceReportsEvictionRetryAndArchivedContextReuse(t *testing.T) {
	a, path := appFixture(t, "member")
	log := captureDebug(a)
	var offline atomic.Bool
	offline.Store(true)
	modelFixture(t, a, path, &offline)
	if err := a.Config.Set(nil, "debug", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	if err := a.Config.Set(nil, "queue_limit", json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	deliver(t, a, 1, message(50, "first"))
	m := message(50, "second")
	m.ID = 2
	deliver(t, a, 2, m)
	if rows := events(t, log, "queue.evicted"); len(rows) != 1 || rows[0]["evicted"] != float64(1) {
		t.Fatalf("eviction not visible: %v", rows)
	}
	now := time.Now()
	runModel(t, a, now)
	rows := events(t, log, "model.retry_scheduled")
	if len(rows) != 1 || rows[0]["status"] != "pending" || rows[0]["retry_at"] == nil {
		t.Fatalf("retry lost: %v", rows)
	}
	if worked, err := a.ProcessModel(context.Background(), now); err != nil || worked {
		t.Fatal("backoff lost")
	}
	if len(events(t, log, "model.started")) != 1 {
		t.Fatal("idle polling flooded model-start logs")
	}
	offline.Store(false)
	runModel(t, a, now.Add(2*time.Second))
	contexts := events(t, log, "context.ready")
	if len(contexts) != 2 || contexts[1]["reused"] != true || len(events(t, log, "profile.started")) != 1 {
		t.Fatalf("context reuse misreported: %v", contexts)
	}
}

func TestDebugTraceExplainsActionDispatchAndProtection(t *testing.T) {
	for _, member := range []string{"member", "administrator"} {
		t.Run(member, func(t *testing.T) {
			var role atomic.Value
			role.Store("member")
			a, path := appFixtureWithMembership(t, func() string { return role.Load().(string) })
			log := captureDebug(a)
			if err := a.Config.Set(nil, "debug", json.RawMessage(`true`)); err != nil {
				t.Fatal(err)
			}
			groupedModel(t, a, path, "enforce")
			deliver(t, a, 1, message(50, "action"))
			runModel(t, a, time.Now())
			role.Store(member)
			if worked, err := a.ProcessAction(context.Background()); err != nil || !worked {
				t.Fatalf("action: %v %v", worked, err)
			}
			if len(events(t, log, "action.preflight")) != 1 {
				t.Fatal("preflight not logged")
			}
			rows := events(t, log, "action.completed")
			if len(rows) != 1 || rows[0]["revision"] != float64(1) || rows[0]["action_id"] == nil {
				t.Fatalf("action correlation lost: %v", rows)
			}
			want := "succeeded"
			dispatches := 1
			if member == "administrator" {
				want = "skipped"
				dispatches = 0
			}
			if rows[0]["status"] != want || len(events(t, log, "action.dispatching")) != dispatches {
				t.Fatalf("action outcome wrong: %v", rows)
			}
		})
	}
}

func TestProfileDebugLogsAreGatedAndRedactCredentials(t *testing.T) {
	a, _ := appFixture(t, "member")
	log := captureDebug(a)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"ok":false,"error_code":400,"description":"failed for 123:PROFILE_SECRET"}`))
	}))
	defer s.Close()
	cfg := a.Config.Snapshot().Telegram
	cfg.APIBase = s.URL
	cfg.ProfileCacheSeconds = 0
	tg, err := telegram.New(cfg, "123:PROFILE_SECRET", s.Client(), 0, a.Log)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := a.diagnostics(-100, "message", 1, "revision", 1, "update", 1)
	if profile := tg.Profile(context.Background(), message(50, "sample"), diagnostic); profile.Status != "unavailable" {
		t.Fatal("profile failure lost")
	}
	if log.Len() != 0 {
		t.Fatal("disabled debug logged profile failure")
	}
	if err := a.Config.Set(nil, "debug", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	tg.Profile(context.Background(), message(50, "sample"), diagnostic)
	if rows := events(t, log, "profile.completed"); len(rows) != 1 || rows[0]["status"] != "unavailable" {
		t.Fatalf("profile failure not visible: %v", rows)
	}
	if strings.Contains(log.String(), "123:PROFILE_SECRET") || !strings.Contains(log.String(), "[redacted]") {
		t.Fatal("profile error credential redaction failed")
	}
}

func TestProfileCacheHitsAreVisible(t *testing.T) {
	a, path := appFixture(t, "member")
	log := captureDebug(a)
	modelFixture(t, a, path, nil)
	if err := a.Config.Set(nil, "debug", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		m := message(50, "sample")
		m.ID = i
		deliver(t, a, int64(i), m)
		runModel(t, a, time.Now())
	}
	if len(events(t, log, "profile.started")) != 1 || len(events(t, log, "profile.cache_hit")) != 1 {
		t.Fatal("cache activity misreported")
	}
}

func TestDisablingDebugDuringInferenceSuppressesSubsequentEvents(t *testing.T) {
	a, path := appFixture(t, "member")
	log := captureDebug(a)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Questions map[string]config.Question `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if err := a.Config.Set(nil, "debug", json.RawMessage(`false`)); err != nil {
			t.Error(err)
		}
		answers := map[string]any{}
		for id := range request.Questions {
			answers[id] = map[string]any{"type": "noul", "noul": .01}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "m", "answers": answers})
	}))
	defer s.Close()
	c := a.Config.Snapshot()
	c.Model.BaseURL = s.URL
	c.Base.Debug = true
	raw, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Config.Reload(); err != nil {
		t.Fatal(err)
	}
	deliver(t, a, 1, message(50, "sample"))
	runModel(t, a, time.Now())
	if len(events(t, log, "model.started")) != 1 || len(events(t, log, "model.completed")) != 0 || len(events(t, log, "model.answers")) != 0 {
		t.Fatal("logger cached an obsolete debug setting")
	}
	if scalar(t, path, `SELECT count(*) FROM evaluations`) != 1 {
		t.Fatal("debug change affected persistence")
	}
}

func TestConfigurationDebugReportsQueueResizeAndRejectsWithoutValues(t *testing.T) {
	var role atomic.Value
	role.Store("member")
	a, _ := appFixtureWithMembership(t, func() string { return role.Load().(string) })
	log := captureDebug(a)
	if err := a.Config.Set(nil, "debug", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		m := message(50, "sample")
		m.ID = i
		deliver(t, a, int64(i), m)
	}
	role.Store("administrator")
	m := message(42, `/as_set queue_limit 1`)
	m.ID = 3
	deliver(t, a, 3, m)
	updated := events(t, log, "config.updated")
	if len(updated) != 1 || updated[0]["scope"] != "group" || updated[0]["setting"] != "queue_limit" || updated[0]["actor"] != float64(42) {
		t.Fatalf("config audit lost: %v", updated)
	}
	resized := events(t, log, "queue.resized")
	if len(resized) != 1 || resized[0]["evicted"] != float64(1) || resized[0]["queue_limit"] != float64(1) {
		t.Fatalf("resize outcome lost: %v", resized)
	}
	m = message(42, `/as_set condition "PRIVATE_CONFIG_SECRET"`)
	m.ID = 4
	deliver(t, a, 4, m)
	if len(events(t, log, "config.rejected")) != 1 {
		t.Fatal("rejected setting not logged")
	}
	if strings.Contains(log.String(), "PRIVATE_CONFIG_SECRET") {
		t.Fatal("command arguments leaked to logs")
	}
}

func TestConfigurationAuditSurvivesDisablingDebug(t *testing.T) {
	a, _ := appFixture(t, "administrator")
	log := captureDebug(a)
	if err := a.Config.Set(nil, "debug", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	deliver(t, a, 1, message(42, `/as_set debug false`))
	updated := events(t, log, "config.updated")
	if len(updated) != 1 || updated[0]["level"] != "INFO" {
		t.Fatal("debug disable hid its own committed configuration change")
	}
	log.Reset()
	m := message(50, "quiet message")
	m.ID = 2
	deliver(t, a, 2, m)
	if strings.Contains(log.String(), `"level":"DEBUG"`) {
		t.Fatal("debug remained enabled")
	}
}

func TestScoreDiagnosticsWhitelistFieldsAndKeepExtraTextOnlyInArchive(t *testing.T) {
	a, path := appFixture(t, "member")
	log := captureDebug(a)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"m","answers":{"risk":{"type":"score","score":0.9,"probabilities":{"0":0.1,"1":0.9},"confidence":0.9,"choice":"PRIVATE_MODEL_TEXT_PAYLOAD"}}}`))
	}))
	defer s.Close()
	c := a.Config.Snapshot()
	c.Model.BaseURL = s.URL
	c.Base.Debug = true
	c.Questions = map[string]config.Question{"risk": {Type: "score", Instructions: "risk?", Criteria: json.RawMessage(`["low","high"]`)}}
	c.Base.Rules = map[string]config.Rule{}
	c.Base.Scoring = &config.Scoring{Rules: []config.ScoreRule{{Name: "risk", When: "s.risk > 0.8", Add: 1}}}
	c.Base.Condition = "score >= 1"
	saveScoringConfig(t, a, path, c)
	deliver(t, a, 1, message(50, "PRIVATE_ORIGINAL_MESSAGE"))
	runModel(t, a, time.Now())
	if strings.Contains(log.String(), "PRIVATE_MODEL_TEXT_PAYLOAD") || strings.Contains(log.String(), "PRIVATE_ORIGINAL_MESSAGE") {
		t.Fatal("unused answer text leaked to debug logs")
	}
	rows := events(t, log, "model.answers")
	if len(rows) != 1 {
		t.Fatal("score diagnostics missing")
	}
	answers := rows[0]["answers"].(map[string]any)
	risk := answers["risk"].(map[string]any)
	if risk["score"] != .9 || risk["confidence"] != .9 || risk["status"] != "answered" {
		t.Fatalf("numeric diagnostics lost: %v", risk)
	}
	if _, exists := risk["choice"]; exists {
		t.Fatal("score summary included a choice field")
	}
	if scalar(t, path, `SELECT count(*) FROM attempts WHERE json_extract(response,'$.answers.risk.choice')='PRIVATE_MODEL_TEXT_PAYLOAD'`) != 1 {
		t.Fatal("full response evidence was discarded")
	}
}
