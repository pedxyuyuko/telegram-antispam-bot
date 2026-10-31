package app

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"telegram-antispam-bot/internal/store"
	"telegram-antispam-bot/internal/telegram"
)

func TestOwnerCanReadAndEditBlacklistDetailsInPrivateChat(t *testing.T) {
	var mu sync.Mutex
	var replies []string
	a, path := appFixture(t, "member", func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "sendMessage") {
			r.ParseMultipartForm(1 << 20)
			mu.Lock()
			replies = append(replies, r.FormValue("text"))
			mu.Unlock()
		}
	})
	cmd := message(9, "/as_botban 200")
	cmd.Chat = telegram.Chat{ID: 9, Type: "private"}
	deliver(t, a, 1, cmd)
	cmd.ID = 2
	cmd.Text = `/as_botedit 200 {"note":"资源 推广","active":false}`
	deliver(t, a, 2, cmd)
	entry, err := a.Store.BotBlacklist(context.Background(), 200)
	if err != nil || entry.Active || entry.Note != "资源 推广" {
		t.Fatalf("edit failed: %+v %v", entry, err)
	}
	cmd.ID = 3
	cmd.Text = "/as_botget 200"
	deliver(t, a, 3, cmd)
	cmd.ID = 4
	cmd.Text = "/as_botlist inactive"
	deliver(t, a, 4, cmd)
	mu.Lock()
	text := strings.Join(replies, "\n")
	mu.Unlock()
	for _, want := range []string{"200", "资源 推广", "停用"} {
		if !strings.Contains(text, want) {
			t.Errorf("detail/list omitted %q", want)
		}
	}
	if scalar(t, path, `SELECT count(*) FROM revisions WHERE status='command'`) != 4 {
		t.Fatal("CRUD commands were not archived as commands")
	}
}

func TestBlacklistEditRejectsAmbiguousOrUnknownFields(t *testing.T) {
	a, path := appFixture(t, "member")
	ctx := context.Background()
	if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
		t.Fatal(err)
	}
	for i, payload := range []string{`{}`, `{"name":"spoof"}`, `{"note":null}`, `{"active":null}`, `{"note":1}`, `{"active":"false"}`, `{"active":false,"active":true}`, `{"note":"a"} {"active":false}`, `{"active":`, `{"note":"` + strings.Repeat("字", 1001) + `"}`} {
		cmd := message(9, "/as_botedit 200 "+payload)
		cmd.ID = i + 1
		deliver(t, a, int64(i+1), cmd)
	}
	entry, err := a.Store.BotBlacklist(ctx, 200)
	if err != nil || !entry.Active || entry.Note != "" || entry.Generation != 1 {
		t.Fatalf("invalid edit changed entry: %+v %v", entry, err)
	}
	if scalar(t, path, `SELECT count(*) FROM bot_blacklist_events`) != 1 {
		t.Fatal("invalid edit created audit mutation")
	}
}

func TestNonOwnerCannotReadOrEditBlacklistDetails(t *testing.T) {
	for _, role := range []string{"member", "administrator"} {
		t.Run(role, func(t *testing.T) {
			var mu sync.Mutex
			var replies []string
			a, _ := appFixture(t, role, func(r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "sendMessage") {
					r.ParseMultipartForm(1 << 20)
					mu.Lock()
					replies = append(replies, r.FormValue("text"))
					mu.Unlock()
				}
			})
			ctx := context.Background()
			if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
				t.Fatal(err)
			}
			note := "PRIVATE_NOTE"
			if _, err := a.Store.EditBotBlacklist(ctx, 200, store.BlacklistEdit{Note: &note}, 9, 9002); err != nil {
				t.Fatal(err)
			}
			deliver(t, a, 1, message(42, "/as_botget 200"))
			cmd := message(42, `/as_botedit 200 {"note":"hijacked","active":false}`)
			cmd.ID = 2
			deliver(t, a, 2, cmd)
			entry, err := a.Store.BotBlacklist(ctx, 200)
			if err != nil || !entry.Active || entry.Note != note {
				t.Fatal("non-owner changed global entry")
			}
			mu.Lock()
			text := strings.Join(replies, "\n")
			mu.Unlock()
			if strings.Contains(text, note) {
				t.Fatal("private blacklist note disclosed")
			}
		})
	}
}

func TestBlacklistNoteEditKeepsPendingDeletionValid(t *testing.T) {
	a, _, probe := botFixture(t, "member", "enforce")
	ctx := context.Background()
	if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
		t.Fatal(err)
	}
	bot := message(200, "known bot")
	bot.From.IsBot = true
	deliver(t, a, 1, bot)
	cmd := message(9, `/as_botedit 200 {"note":"still blocked"}`)
	cmd.ID = 2
	deliver(t, a, 2, cmd)
	entry, _ := a.Store.BotBlacklist(ctx, 200)
	if entry.Note != "still blocked" || entry.Generation != 1 {
		t.Fatal("note edit changed enforcement generation")
	}
	doAction(t, a)
	if len(probe.deletes()) != 1 {
		t.Fatal("note edit cancelled deletion")
	}
}

func TestBlacklistEditCanUseRepliedBotAndClearNote(t *testing.T) {
	a, _ := appFixture(t, "member")
	ctx := context.Background()
	if _, err := a.Store.SetBotBlocked(ctx, store.BotIdentity{ID: 200}, true, 9, 9001); err != nil {
		t.Fatal(err)
	}
	bot := message(200, "bot")
	bot.From.IsBot = true
	cmd := message(9, `/as_botedit {"note":"temporary"}`)
	cmd.Reply = bot
	deliver(t, a, 1, cmd)
	entry, _ := a.Store.BotBlacklist(ctx, 200)
	if entry.Note != "temporary" {
		t.Fatal("reply edit failed")
	}
	cmd.ID = 2
	cmd.Text = `/as_botedit 200 {"note":""}`
	cmd.Reply = nil
	deliver(t, a, 2, cmd)
	entry, _ = a.Store.BotBlacklist(ctx, 200)
	if entry.Note != "" {
		t.Fatal("empty note did not clear note")
	}
}
