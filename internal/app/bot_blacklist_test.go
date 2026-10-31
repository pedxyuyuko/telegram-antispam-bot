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

func TestOwnerManagesGlobalBotBlacklistInPrivateChat(t *testing.T) {
	a, path := appFixture(t, "member")
	cmd := message(9, "/as_botban 200")
	cmd.Chat = telegram.Chat{ID: 9, Type: "private"}
	deliver(t, a, 1, cmd)
	entry, err := a.Store.BotBlacklist(context.Background(), 200)
	if err != nil || entry == nil || !entry.Active {
		t.Fatalf("owner private add failed: %+v %v", entry, err)
	}
	cmd.ID = 2
	cmd.Text = "/as_botunban 200"
	deliver(t, a, 2, cmd)
	entry, err = a.Store.BotBlacklist(context.Background(), 200)
	if err != nil || entry == nil || entry.Active {
		t.Fatal("owner removal failed")
	}
	if scalar(t, path, `SELECT count(*) FROM revisions WHERE status='command'`) != 2 {
		t.Fatal("global commands were not archived as commands")
	}
}

func TestGroupAdministratorCannotChangeGlobalBotBlacklist(t *testing.T) {
	for _, role := range []string{"member", "administrator"} {
		t.Run(role, func(t *testing.T) {
			a, path := appFixture(t, role)
			deliver(t, a, 1, message(42, "/as_botban 200"))
			if scalar(t, path, `SELECT count(*) FROM bot_blacklist`) != 0 {
				t.Fatal("non-owner changed global blacklist")
			}
		})
	}
}

func TestOwnerCanUseRepliedBotAndKnownUsername(t *testing.T) {
	a, _ := appFixture(t, "member")
	bot := message(200, "previous bot message")
	bot.From.IsBot = true
	bot.From.Username = "advert_bot"
	bot.From.FirstName = "Advert"
	cmd := message(9, "/as_botban")
	cmd.Reply = bot
	deliver(t, a, 1, cmd)
	entry, err := a.Store.BotBlacklist(context.Background(), 200)
	if err != nil || entry == nil || !entry.Active || entry.Username != "advert_bot" {
		t.Fatalf("reply add: %+v %v", entry, err)
	}
	cmd.ID = 2
	cmd.Text = "/as_botunban @ADVERT_BOT"
	cmd.Reply = nil
	deliver(t, a, 2, cmd)
	entry, err = a.Store.BotBlacklist(context.Background(), 200)
	if err != nil || entry.Active {
		t.Fatal("username removal failed")
	}
}

func TestOwnerBlacklistListingAndSelfProtection(t *testing.T) {
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
	if _, err := a.Store.SetBotBlocked(context.Background(), store.BotIdentity{ID: 200, Username: "advert_bot"}, true, 9, 9001); err != nil {
		t.Fatal(err)
	}
	deliver(t, a, 1, message(9, "/as_botlist"))
	mu.Lock()
	joined := strings.Join(replies, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "advert_bot") || !strings.Contains(joined, "200") {
		t.Fatal("global list did not show bot identity")
	}
	cmd := message(9, "/as_botban 123")
	cmd.ID = 2
	deliver(t, a, 2, cmd)
	if scalar(t, path, `SELECT count(*) FROM bot_blacklist WHERE bot_id=123`) != 0 {
		t.Fatal("self bot added to blacklist")
	}
}
