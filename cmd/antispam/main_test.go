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
	if len(c.QuestionGroups) != 4 {
		t.Fatalf("want four focused question groups, got %d", len(c.QuestionGroups))
	}
	for _, group := range []string{"content", "profile", "target", "join"} {
		p := c.Base
		p.Questions = []string{group + ".*"}
		questions, err := c.SelectedQuestions(p)
		want := map[string]int{"content": 6, "profile": 7, "target": 6, "join": 7}[group]
		if err != nil || len(questions) != want {
			t.Fatalf("question group %s has %d questions, want %d: %v", group, len(questions), want, err)
		}
		anime := group + ".fictional"
		if group == "target" {
			anime = "target.sexual_anime"
		}
		if questions[group+".sexual"].Type != "noul" || questions[anime].Type != "noul" {
			t.Fatalf("%s lacks the sexual and fictional dimensions", group)
		}
		for id, q := range questions {
			if len(q.Criteria) == 0 {
				t.Fatalf("question %s lost its answer criteria", id)
			}
			if group == "content" && (!strings.Contains(q.Instructions, "图片内容") || !bytes.Contains(q.Criteria, []byte("图片"))) {
				t.Fatalf("question %s omits visible image material", id)
			}
		}
	}
	questions, err := c.SelectedQuestions(c.Base)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 19 {
		t.Fatalf("want nineteen selected questions, got %d", len(questions))
	}
	p := map[string]float64{}
	for id := range questions {
		p[id] = .01
	}
	p["content.solicit"], p["content.advert"] = .85, .85
	e, err := decision.EvaluatePolicy(questions, c.Base, decision.Result{Probabilities: p})
	if err != nil || e.Score != 140 || e.Condition != expression.True {
		t.Fatalf("generated example lost custom scoring: %+v %v", e, err)
	}
}

func TestExampleJoinGateStartsDisabledAndScoresIndependentProfileAnswers(t *testing.T) {
	var out, logs bytes.Buffer
	if err := run(context.Background(), []string{"-example"}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	c, err := config.Decode(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if c.JoinGate == nil {
		t.Fatal("join gate missing from example")
	}
	p := c.ForJoinGroup(-100)
	if p.Enabled || p.Mode != "record" {
		t.Fatal("example enables join punishment")
	}
	if r := p.Rules["delete_join_message"]; !r.Enabled || r.Action != "delete" || r.Count != 1 {
		t.Fatal("example lacks matched join-message cleanup")
	}
	q, err := c.SelectedQuestions(p.Policy)
	if err != nil || len(q) != 7 {
		t.Fatal("join question selection missing")
	}
	// Replay of 146 join profiles: 8/8 known spam, 0/6 controls, 4/132 archive
	// joiners (all spam). Plain shop or commission bios are not removed.
	for _, tc := range []struct {
		name  string
		p     map[string]float64
		score float64
	}{
		{"solicited_illicit", map[string]float64{"join.solicit": .85, "join.illicit": .85}, 100},
		{"solicited_money_lure", map[string]float64{"join.solicit": .85, "join.money_lure": .85}, 100},
		{"solicited_real_sexual", map[string]float64{"join.solicit": .85, "join.sexual": .85, "join.fictional": .84999}, 100},
		{"solicited_fictional_sexual", map[string]float64{"join.solicit": .99, "join.sexual": .99, "join.fictional": .85}, 0},
		{"plain_shop_or_commission", map[string]float64{"join.solicit": .99, "join.advert": .99}, 0},
		{"minor_alone", map[string]float64{"join.minor": .85, "join.fictional": .99}, 100},
		{"minor_below", map[string]float64{"join.minor": .84999}, 0},
		{"illicit_name_alone", map[string]float64{"join.illicit": .90}, 100},
		{"illicit_name_below", map[string]float64{"join.illicit": .89999}, 0},
		{"solicitation_below", map[string]float64{"join.solicit": .84999, "join.illicit": .89, "join.money_lure": .99}, 0},
		{"one_rule_per_join", map[string]float64{"join.solicit": .99, "join.illicit": .99, "join.minor": .99}, 100},
	} {
		probabilities := map[string]float64{}
		for id := range q {
			probabilities[id] = .01
		}
		for id, v := range tc.p {
			probabilities[id] = v
		}
		e, err := decision.EvaluatePolicy(q, p.Policy, decision.Result{Probabilities: probabilities})
		if err != nil || e.Score != tc.score {
			t.Fatalf("%s: join scoring=%v %v", tc.name, e.Score, err)
		}
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
		wantScore     float64
	}{
		// Message rule: solicitation plus any one business signal. Replay of
		// 783 archived messages: 262/262 ad-bot posts, 2/400 normal messages.
		{"advert_without_solicitation", map[string]float64{"content.advert": .99, "content.illicit": .99, "content.sexual": .99, "content.minor": .99}, 0},
		{"solicitation_without_business", map[string]float64{"content.solicit": .99}, 40},
		{"solicited_advert_boundary", map[string]float64{"content.solicit": .85, "content.advert": .85}, 140},
		{"solicitation_below_boundary", map[string]float64{"content.solicit": .84999, "content.advert": .99, "content.illicit": .99}, 0},
		{"solicited_illicit_service", map[string]float64{"content.solicit": .85, "content.illicit": .85}, 140},
		{"solicited_minor_sexualization", map[string]float64{"content.solicit": .85, "content.minor": .85}, 140},
		{"solicited_real_sexual_content", map[string]float64{"content.solicit": .85, "content.sexual": .85, "content.fictional": .84999}, 140},
		{"fictional_sexual_content_is_exempt", map[string]float64{"content.solicit": .99, "content.sexual": .99, "content.fictional": .85}, 40},
		{"fiction_does_not_hide_advert", map[string]float64{"content.solicit": .85, "content.advert": .85, "content.fictional": .99}, 140},
		{"fiction_does_not_hide_minor_sexualization", map[string]float64{"content.solicit": .85, "content.minor": .85, "content.fictional": .99}, 140},
		{"illicit_alone_is_not_scored", map[string]float64{"content.illicit": .99}, 0},
		{"business_signals_below_boundary", map[string]float64{"content.solicit": .99, "content.advert": .84999, "content.illicit": .84999, "content.minor": .84999, "content.sexual": .84999}, 40},
		{"one_message_rule_per_message", map[string]float64{"content.solicit": .99, "content.advert": .99, "content.illicit": .99, "content.minor": .99, "content.sexual": .99}, 140},
		// Sender profile: business that warrants action on its own. Replay of
		// 370 senders: 4 users, all spam (two payment agents, an ad-service
		// account and a child-abuse lure).
		{"profile_solicited_illicit", map[string]float64{"profile.solicit": .85, "profile.illicit": .85}, 100},
		{"profile_solicited_money_lure", map[string]float64{"profile.solicit": .85, "profile.money_lure": .85}, 100},
		{"profile_solicited_real_sexual", map[string]float64{"profile.solicit": .85, "profile.sexual": .85, "profile.fictional": .84999}, 100},
		{"profile_fictional_sexual_is_exempt", map[string]float64{"profile.solicit": .99, "profile.sexual": .99, "profile.fictional": .85}, 0},
		{"profile_minor_alone", map[string]float64{"profile.minor": .95}, 100},
		{"profile_minor_below", map[string]float64{"profile.minor": .94999, "profile.fictional": .99}, 0},
		{"profile_illicit_without_solicitation", map[string]float64{"profile.solicit": .84999, "profile.illicit": .99}, 0},
		// Ordinary selling in a bio needs the message itself to solicit.
		{"profile_advert_alone", map[string]float64{"profile.solicit": .99, "profile.advert": .99}, 60},
		{"profile_advert_with_soliciting_message", map[string]float64{"profile.solicit": .85, "profile.advert": .85, "content.solicit": .85}, 100},
		{"soliciting_message_alone", map[string]float64{"content.solicit": .99}, 40},
		{"profile_business_and_message_rule", map[string]float64{"profile.solicit": .99, "profile.illicit": .99, "content.solicit": .99, "content.advert": .99}, 240},
		{"target_answers_are_observational", map[string]float64{"target.invocation": .99, "target.promotion": .99, "target.money": .99, "target.sexual": .99, "target.sexual_anime": .99, "target.earnings": .99}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := map[string]float64{}
			for id := range q {
				p[id] = .01
			}
			for id, value := range tc.probabilities {
				p[id] = value
			}
			e, err := decision.EvaluatePolicy(q, c.Base, decision.Result{Probabilities: p})
			want := expression.False
			if tc.wantScore >= 100 {
				want = expression.True
			}
			if err != nil || e.Score != tc.wantScore || e.Condition != want {
				t.Fatalf("unexpected example behavior: %+v %v", e, err)
			}
		})
	}
	for _, id := range []int64{-1001234567890, -1009876543210} {
		p := c.ForGroup(id)
		selected, err := c.SelectedQuestions(p)
		if err != nil || len(selected) != 19 {
			t.Fatalf("group %d lost the nineteen-question policy: %v", id, err)
		}
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
		case strings.HasSuffix(r.URL.Path, "getChatMember"):
			fmt.Fprint(w, `{"ok":true,"result":{"status":"member","user":{"id":42,"first_name":"Alice"}}}`)
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
		json.NewEncoder(w).Encode(map[string]any{"model": request.Model, "answers": answers, "private": "MODEL_BODY_SENTINEL"})
	}))
	defer model.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := config.Defaults()
	c.OwnerIDs = []int64{9}
	c.Model.Name = "m"
	c.Model.BaseURL = model.URL
	c.Base.Debug = true
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
	for _, private := range []string{"public biography", "preserve me", "MODEL_BODY_SENTINEL"} {
		if strings.Contains(logs.String(), private) {
			t.Fatalf("payload leaked into logs: %s", private)
		}
	}
	var started, archived int
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["level"] == "DEBUG" && row["event"] == "model.started" {
			started++
		}
		if row["level"] == "DEBUG" && row["event"] == "model.archived" {
			archived++
		}
	}
	if started != 2 || archived != 2 {
		t.Fatalf("CLI filtered debug logs: starts=%d archives=%d", started, archived)
	}
	if err := db.QueryRow(`SELECT count(*) FROM attempts WHERE json_extract(response,'$.private')='MODEL_BODY_SENTINEL'`).Scan(&n); err != nil || n != 2 {
		t.Fatal("raw model evidence lost while suppressing payload logs")
	}
}
