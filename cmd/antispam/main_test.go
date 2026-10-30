package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/expression"
)

func TestExampleAndOfflineValidation(t *testing.T) {
	var out, logs bytes.Buffer
	if err := run(context.Background(), []string{"-example"}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	c, err := config.Decode(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if c.Base.Mode != "record" {
		t.Fatal("example enables punishments")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(context.Background(), []string{"-config", path, "-check"}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "valid") {
		t.Fatal("missing validation result")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "data")); !os.IsNotExist(err) {
		t.Fatal("offline validation opened a database")
	}
}

func TestExampleProvidesGroupedScoringOutsideRepository(t *testing.T) {
	t.Chdir(t.TempDir())
	var out, logs bytes.Buffer
	if err := run(context.Background(), []string{"-example"}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	if _, exists := root["questions"]; exists {
		t.Fatal("generated example includes the obsolete top-level question set")
	}
	c, err := config.Decode(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Questions) != 0 || c.Base.Scoring == nil || c.Base.Condition == "" {
		t.Fatal("example still uses the legacy flat-question policy")
	}
	for _, removed := range []string{"benign.discussion", "benign.participation"} {
		p := c.Base
		p.Questions = []string{removed}
		if _, err := c.SelectedQuestions(p); err == nil {
			t.Fatalf("removed question %s remains selectable in the example", removed)
		}
	}
	for _, group := range []string{"profile", "message", "entry", "crosslink", "benign", "decision"} {
		p := c.Base
		p.Questions = []string{group + ".*"}
		questions, err := c.SelectedQuestions(p)
		want := 4
		if group == "benign" {
			want = 2
		}
		if err != nil || len(questions) != want {
			t.Fatalf("incomplete question group %s: %v %v", group, questions, err)
		}
		for id, q := range questions {
			if len(q.Criteria) == 0 {
				t.Fatalf("question %s lost its answer criteria", id)
			}
		}
	}
	questions, err := c.SelectedQuestions(c.Base)
	if err != nil {
		t.Fatal(err)
	}
	e, err := decision.EvaluatePolicy(questions, c.Base, decision.Result{Probabilities: map[string]float64{"profile.promotion": .99, "message.promotion": .99}})
	if err != nil || e.Score != 70 || e.Condition != expression.True {
		t.Fatalf("generated example lost custom scoring: %+v %v", e, err)
	}
}

func TestGroupedExampleValidatesAndScoresRepresentativeMessages(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "scoring.json")
	var out, logs bytes.Buffer
	if err := run(context.Background(), []string{"-config", path, "-check"}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	q, err := c.SelectedQuestions(c.Base)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		probabilities map[string]float64
		want          expression.Truth
	}{
		{"profile_only", map[string]float64{"profile.promotion": .99}, expression.Unknown},
		{"promotion", map[string]float64{"profile.promotion": .99, "message.promotion": .99}, expression.True},
		{"warning", map[string]float64{"profile.promotion": .99, "message.promotion": .99, "benign.warning": .99, "decision.fraud": .01}, expression.False},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := decision.EvaluatePolicy(q, c.Base, decision.Result{Probabilities: tc.probabilities})
			if err != nil || e.Condition != tc.want {
				t.Fatalf("unexpected example behavior: %+v %v", e, err)
			}
		})
	}
}

func TestMissingTokenStopsBeforeDatabaseCreation(t *testing.T) {
	t.Setenv("TEST_EMPTY_BOT_TOKEN", "")
	c := config.Defaults()
	c.OwnerIDs = []int64{9}
	c.Model.Name = "m"
	c.Telegram.TokenEnv = "TEST_EMPTY_BOT_TOKEN"
	raw, _ := json.Marshal(c)
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, raw, 0600)
	var out, logs bytes.Buffer
	if err := run(context.Background(), []string{"-config", path}, &out, &logs); err == nil {
		t.Fatal("started with an empty token")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "data")); !os.IsNotExist(err) {
		t.Fatal("missing credentials created runtime artifacts")
	}
}

func TestPollingToDatabaseToModelEndToEnd(t *testing.T) {
	t.Setenv("TEST_BOT_TOKEN", "123:local-test")
	var polls atomic.Int32
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"Guard","username":"guard_bot"}}`)
		case strings.HasSuffix(r.URL.Path, "getWebhookInfo"):
			fmt.Fprint(w, `{"ok":true,"result":{"url":""}}`)
		case strings.HasSuffix(r.URL.Path, "getChat"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":42,"type":"private","bio":"public biography"}}`)
		case strings.HasSuffix(r.URL.Path, "getUpdates"):
			if polls.Add(1) == 1 {
				fmt.Fprint(w, `{"ok":true,"result":[{"update_id":500,"message":{"message_id":7,"date":1,"chat":{"id":-100,"type":"supergroup"},"from":{"id":42,"first_name":"Alice"},"text":"original","reply_markup":{"inline_keyboard":[[{"text":"button","url":"https://example.test/ad"}]]}}},{"update_id":501,"edited_message":{"message_id":7,"date":1,"edit_date":2,"chat":{"id":-100,"type":"supergroup"},"from":{"id":42,"first_name":"Alice"},"text":"edited","future_field":"preserve me"}}]}`)
			} else {
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
					fmt.Fprint(w, `{"ok":true,"result":[]}`)
				}
			}
		default:
			t.Errorf("unexpected Telegram action: %s", r.URL.Path)
			fmt.Fprint(w, `{"ok":false,"description":"unexpected"}`)
		}
	}))
	defer tg.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model     string                     `json:"model"`
			State     json.RawMessage            `json:"state"`
			Questions map[string]config.Question `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if !strings.Contains(string(request.State), "public biography") {
			t.Error("profile omitted")
		}
		answers := map[string]any{}
		for id := range request.Questions {
			answers[id] = map[string]any{"type": "noul", "noul": 0.1}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": request.Model, "answers": answers})
	}))
	defer model.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := config.Defaults()
	c.OwnerIDs = []int64{9}
	c.Model.Name = "m"
	c.Model.BaseURL = model.URL
	c.Telegram.APIBase = tg.URL
	c.Telegram.TokenEnv = "TEST_BOT_TOKEN"
	raw, _ := json.Marshal(c)
	os.WriteFile(path, raw, 0600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, logs bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", path}, &out, &logs) }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	complete := false
	for !complete {
		select {
		case err := <-done:
			t.Fatalf("program exited: %v", err)
		case <-deadline.C:
			cancel()
			<-done
			t.Fatalf("end-to-end timeout: %s", logs.String())
		case <-tick.C:
			dbpath := filepath.Join(dir, "data", "bot.sqlite")
			if _, err := os.Stat(dbpath); err != nil {
				continue
			}
			db, err := sql.Open("sqlite", dbpath)
			if err != nil {
				continue
			}
			var n int
			err = db.QueryRow(`SELECT count(*) FROM attempts WHERE error=''`).Scan(&n)
			db.Close()
			complete = err == nil && n == 2
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "data", "bot.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM revisions`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("versions lost: %d %v", n, err)
	}
	var stored string
	db.QueryRow(`SELECT raw FROM revisions ORDER BY id DESC LIMIT 1`).Scan(&stored)
	if !strings.Contains(stored, "preserve me") {
		t.Fatal("unknown Telegram fields were discarded")
	}
	if strings.Contains(logs.String(), "123:local-test") {
		t.Fatal("token leaked into logs")
	}
}
