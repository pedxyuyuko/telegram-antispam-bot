package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"telegram-antispam-bot/internal/config"
)

func TestBlacklistNoteEditPreservesIdentityAndEnforcementGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bot := BotIdentity{ID: 200, Username: "ad_bot", Name: "Ad"}
	if _, err := s.SetBotBlocked(ctx, bot, true, 9, 101); err != nil {
		t.Fatal(err)
	}
	before, _ := s.BotBlacklist(ctx, 200)
	note := "人工复核：资源广告"
	change, err := s.EditBotBlacklist(ctx, 200, BlacklistEdit{Note: &note}, 9, 102)
	if err != nil || !change.Changed {
		t.Fatalf("edit: %+v %v", change, err)
	}
	after, _ := s.BotBlacklist(ctx, 200)
	if after.Note != note || after.Generation != before.Generation || after.Active != before.Active || after.BotIdentity != before.BotIdentity || after.Source != before.Source {
		t.Fatalf("note edit changed enforcement identity: %+v", after)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM bot_blacklist_events WHERE command_update_id=102 AND kind='manual_edit' AND json_extract(detail,'$.before.note')='' AND json_extract(detail,'$.after.note')=?`, note).Scan(&count)
	if count != 1 {
		t.Fatal("note change audit missing")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, _ = s.BotBlacklist(ctx, 200)
	if after.Note != note {
		t.Fatal("note lost after restart")
	}
	replacement := "replayed change"
	change, err = s.EditBotBlacklist(ctx, 200, BlacklistEdit{Note: &replacement}, 9, 102)
	if err != nil || !change.Duplicate {
		t.Fatalf("duplicate edit: %+v %v", change, err)
	}
	after, _ = s.BotBlacklist(ctx, 200)
	if after.Note != note {
		t.Fatal("duplicate command replaced newer state")
	}
}

func TestBlacklistEditStatusRetainsRemovalBoundaryAndNotes(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	add(t, s, 1, -100, 1, 50, config.Defaults().Base)
	if _, err := s.SetBotBlocked(ctx, BotIdentity{ID: 200}, true, 9, 101); err != nil {
		t.Fatal(err)
	}
	active := false
	note := "暂停"
	if _, err := s.EditBotBlacklist(ctx, 200, BlacklistEdit{Active: &active, Note: &note}, 9, 102); err != nil {
		t.Fatal(err)
	}
	entry, _ := s.BotBlacklist(ctx, 200)
	if entry.Active || entry.Generation != 2 || entry.ReleasedThroughRevision < 1 || entry.Note != note {
		t.Fatalf("disable: %+v", entry)
	}
	active = true
	if _, err := s.EditBotBlacklist(ctx, 200, BlacklistEdit{Active: &active}, 9, 103); err != nil {
		t.Fatal(err)
	}
	entry, _ = s.BotBlacklist(ctx, 200)
	if !entry.Active || entry.Generation != 3 || entry.Note != note {
		t.Fatalf("enable: %+v", entry)
	}
	if _, err := s.SetBotBlocked(ctx, BotIdentity{ID: 200}, false, 9, 104); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditBotBlacklist(ctx, 200, BlacklistEdit{Active: &active}, 9, 103); err != nil {
		t.Fatal(err)
	}
	entry, _ = s.BotBlacklist(ctx, 200)
	if entry.Active || entry.Note != note {
		t.Fatal("old edit overrode removal or note was lost")
	}
}

func TestBlacklistEditRejectsMissingEntriesAndOversizedNotes(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	note := "note"
	if _, err := s.EditBotBlacklist(ctx, 999, BlacklistEdit{Note: &note}, 9, 101); !errors.Is(err, ErrBlacklistEntryNotFound) {
		t.Fatalf("missing entry: %v", err)
	}
	if _, err := s.SetBotBlocked(ctx, BotIdentity{ID: 200}, true, 9, 102); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditBotBlacklist(ctx, 200, BlacklistEdit{}, 9, 103); err == nil {
		t.Fatal("empty edit accepted")
	}
	long := strings.Repeat("字", 1001)
	if _, err := s.EditBotBlacklist(ctx, 200, BlacklistEdit{Note: &long}, 9, 104); err == nil {
		t.Fatal("oversized note accepted")
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM bot_blacklist_events`).Scan(&count)
	if count != 1 {
		t.Fatal("rejected edit changed audit state")
	}
}

func TestBlacklistListSupportsActiveInactiveAndAll(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i, id := range []int64{200, 300} {
		if _, err := s.SetBotBlocked(ctx, BotIdentity{ID: id}, true, 9, int64(100+i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetBotBlocked(ctx, BotIdentity{ID: 200}, false, 9, 102); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status string
		count  int
	}{{"active", 1}, {"inactive", 1}, {"all", 2}} {
		entries, total, err := s.ListBotBlacklist(ctx, tc.status, 0, 50)
		if err != nil || total != tc.count || len(entries) != tc.count {
			t.Fatalf("list %s: %+v %d %v", tc.status, entries, total, err)
		}
	}
}

func TestVersionThreeBlacklistMetadataMigrationPreservesRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetBotBlocked(context.Background(), BotIdentity{ID: 200, Username: "ad_bot"}, true, 9, 101); err != nil {
		t.Fatal(err)
	}
	before, _ := s.BotBlacklist(context.Background(), 200)
	s.Close()
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`ALTER TABLE bot_blacklist DROP COLUMN note; ALTER TABLE bot_blacklist_events DROP COLUMN detail; PRAGMA user_version=3;`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, _ := s.BotBlacklist(context.Background(), 200)
	if *before != *after {
		t.Fatalf("migration changed blacklist row: before=%+v after=%+v", before, after)
	}
	var version, count int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	s.db.QueryRow(`SELECT count(*) FROM bot_blacklist_events WHERE detail=''`).Scan(&count)
	if version != 10 || count != 1 {
		t.Fatalf("migration version=%d preserved_events=%d", version, count)
	}
}
