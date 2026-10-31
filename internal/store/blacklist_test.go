package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/telegram"
)

func TestBlacklistMigrationCreatesAuditableGlobalState(t *testing.T) {
	s := openTest(t)
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 4 {
		t.Fatalf("blacklist schema version=%d want 4", version)
	}
	for _, name := range []string{"bot_blacklist", "bot_blacklist_events", "blacklist_matches", "action_evidence", "bot_action_events", "bot_invocations"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
}

func TestBlacklistOwnerRemovalAndCommandReplaySurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bot := BotIdentity{ID: 200, Username: "ad_bot", Name: "Ad"}
	change, err := s.SetBotBlocked(ctx, bot, true, 9, 101)
	if err != nil || !change.Changed {
		t.Fatalf("add: %+v %v", change, err)
	}
	entry, err := s.BotBlacklist(ctx, 200)
	if err != nil || entry == nil || !entry.Active || entry.Generation != 1 {
		t.Fatalf("lookup: %+v %v", entry, err)
	}
	if _, err := s.SetBotBlocked(ctx, bot, false, 9, 102); err != nil {
		t.Fatal(err)
	}
	change, err = s.SetBotBlocked(ctx, bot, true, 9, 101)
	if err != nil || !change.Duplicate {
		t.Fatalf("command replay: %+v %v", change, err)
	}
	entry, _ = s.BotBlacklist(ctx, 200)
	if entry.Active {
		t.Fatal("old command reactivated removed bot")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entry, err = s.BotBlacklist(ctx, 200)
	if err != nil || entry.Active || entry.Generation != 2 {
		t.Fatalf("removal lost after restart: %+v %v", entry, err)
	}
	var events int
	s.db.QueryRow(`SELECT count(*) FROM bot_blacklist_events WHERE bot_id=200`).Scan(&events)
	if events != 2 {
		t.Fatalf("events=%d want 2", events)
	}
}

func TestBlacklistListingIsPagedAndExcludesRemovedEntries(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i, id := range []int64{200, 300, 400} {
		if _, err := s.SetBotBlocked(ctx, BotIdentity{ID: id}, true, 9, int64(100+i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetBotBlocked(ctx, BotIdentity{ID: 300}, false, 9, 200); err != nil {
		t.Fatal(err)
	}
	entries, total, err := s.ListBlockedBots(ctx, 1, 1)
	if err != nil || total != 2 || len(entries) != 1 || entries[0].ID != 400 {
		t.Fatalf("list=%+v total=%d err=%v", entries, total, err)
	}
}

func seedDeletion(t *testing.T, s *Store, update, user int64, bot bool, outcome string) int64 {
	t.Helper()
	ctx := context.Background()
	p := config.Defaults().Base
	p.Mode = "enforce"
	if outcome == "recorded" {
		p.Mode = "record"
	}
	m := &telegram.Message{ID: int(update), Chat: telegram.Chat{ID: -100, Type: "supergroup"}, From: &telegram.User{ID: user, IsBot: bot, FirstName: "Sender", Username: "source_bot"}, Text: "archived body", Date: testNow.Unix()}
	raw, _ := json.Marshal(map[string]any{"update_id": update, "message": m})
	if err := s.Capture(ctx, []json.RawMessage{raw}); err != nil {
		t.Fatal(err)
	}
	r, err := s.AddRevision(ctx, Inbox{ID: update, Raw: raw, ReceivedAt: testNow.UnixMilli()}, m, Snapshot{Policy: p, Questions: config.Defaults().Questions}, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkInbox(ctx, update, ""); err != nil {
		t.Fatal(err)
	}
	j, err := s.Claim(ctx, testNow)
	if err != nil || j == nil {
		t.Fatalf("claim: %+v %v", j, err)
	}
	if _, err := s.Finish(ctx, *j, decision.Result{Probabilities: map[string]float64{"violation": .99}}, nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
	if outcome != "recorded" {
		a, err := s.TakeAction(ctx)
		if err != nil || a == nil {
			t.Fatalf("action: %+v %v", a, err)
		}
		if err := s.FinishAction(ctx, a.ID, outcome, ""); err != nil {
			t.Fatal(err)
		}
	}
	return r.ID
}

func TestVersionTwoBootstrapUsesOnlySuccessfulBotDeletions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedDeletion(t, s, 1, 200, true, "succeeded")
	seedDeletion(t, s, 2, 201, false, "succeeded")
	seedDeletion(t, s, 3, 202, true, "failed")
	seedDeletion(t, s, 4, 203, true, "recorded")
	var beforeRaw string
	s.db.QueryRow(`SELECT group_concat(raw,'|') FROM revisions ORDER BY id`).Scan(&beforeRaw)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`DROP TABLE bot_invocations; DROP TABLE bot_action_events; DROP TABLE action_evidence; DROP TABLE blacklist_matches; DROP TABLE bot_blacklist_events; DROP TABLE bot_blacklist; DELETE FROM meta WHERE key='bot_blacklist_initialized'; PRAGMA user_version=2;`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entries, total, err := s.ListBlockedBots(context.Background(), 0, 50)
	if err != nil || total != 1 || len(entries) != 1 || entries[0].ID != 200 || entries[0].Source != "history" {
		t.Fatalf("bootstrap: %+v total=%d err=%v", entries, total, err)
	}
	var afterRaw string
	s.db.QueryRow(`SELECT group_concat(raw,'|') FROM revisions ORDER BY id`).Scan(&afterRaw)
	if beforeRaw != afterRaw {
		t.Fatal("bootstrap changed raw archives")
	}
	var actions, attempts, links int
	s.db.QueryRow(`SELECT count(*) FROM actions`).Scan(&actions)
	s.db.QueryRow(`SELECT count(*) FROM attempts`).Scan(&attempts)
	s.db.QueryRow(`SELECT count(*) FROM bot_invocations`).Scan(&links)
	if actions != 4 || attempts != 4 || links != 0 {
		t.Fatalf("bootstrap rewrote history: actions=%d attempts=%d invocation_links=%d", actions, attempts, links)
	}
}

func TestBotResolutionTracksObservedRenamesAndRejectsKnownHumans(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.SetBotBlocked(ctx, BotIdentity{ID: 200, Username: "old_bot"}, true, 9, 9001); err != nil {
		t.Fatal(err)
	}
	m := &telegram.Message{ID: 1, Chat: telegram.Chat{ID: -100, Type: "supergroup"}, From: &telegram.User{ID: 200, IsBot: true, FirstName: "Bot", Username: "new_bot"}, Text: "renamed"}
	raw, _ := json.Marshal(map[string]any{"update_id": 1, "message": m})
	if err := s.Capture(ctx, []json.RawMessage{raw}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRevision(ctx, Inbox{ID: 1, Raw: raw, ReceivedAt: testNow.UnixMilli()}, m, Snapshot{Policy: config.Defaults().Base, Questions: config.Defaults().Questions}, "ignored"); err != nil {
		t.Fatal(err)
	}
	bot, err := s.ResolveBot(ctx, "@NEW_BOT")
	if err != nil || bot.ID != 200 {
		t.Fatalf("current alias: %+v %v", bot, err)
	}
	if _, err := s.ResolveBot(ctx, "@old_bot"); !errors.Is(err, ErrUnknownBot) {
		t.Fatalf("stale alias accepted: %v", err)
	}
	if _, err := s.SetBotBlocked(ctx, BotIdentity{ID: 200, Username: "old_bot"}, true, 9, 9002); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveBot(ctx, "@old_bot"); !errors.Is(err, ErrUnknownBot) {
		t.Fatal("cached alias overrode the newest captured bot identity")
	}
	add(t, s, 2, -100, 2, 50, config.Defaults().Base)
	if _, err := s.ResolveBot(ctx, "50"); !errors.Is(err, ErrNotBot) {
		t.Fatalf("known human accepted: %v", err)
	}
	bot, err = s.ResolveBot(ctx, "999")
	if err != nil || bot.ID != 999 {
		t.Fatalf("unobserved numeric ID: %+v %v", bot, err)
	}
}
