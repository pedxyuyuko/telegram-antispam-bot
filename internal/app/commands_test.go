package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/store"
	"telegram-antispam-bot/internal/telegram"
)

func appFixture(t *testing.T, member string, observe ...func(*http.Request)) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	dbpath := filepath.Join(dir, "bot.sqlite")
	if err := os.WriteFile(path, []byte(`{"owner_ids":[9],"model":{"name":"m"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(dbpath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, fn := range observe {
			fn(r)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "getChatMember"):
			w.Write([]byte(`{"ok":true,"result":{"status":"` + member + `","user":{"id":42,"first_name":"u"}}}`))
		case strings.HasSuffix(r.URL.Path, "sendMessage"):
			w.Write([]byte(`{"ok":true,"result":{"message_id":100,"chat":{"id":-100,"type":"supergroup"},"date":1}}`))
		case strings.HasSuffix(r.URL.Path, "getChat"):
			w.Write([]byte(`{"ok":true,"result":{"id":42,"type":"private","bio":"profile"}}`))
		default:
			w.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	t.Cleanup(h.Close)
	tgCfg := cfg.Snapshot().Telegram
	tgCfg.APIBase = h.URL
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tg, err := telegram.New(tgCfg, "123:secret", h.Client(), 0, log)
	if err != nil {
		t.Fatal(err)
	}
	tg.Me = &models.User{ID: 123, Username: "guard_bot"}
	return &App{Config: cfg, Store: s, Telegram: tg, Model: &decision.Client{HTTP: h.Client()}, Log: log}, dbpath
}

func deliver(t *testing.T, a *App, id int64, m *telegram.Message) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"update_id": id, "message": m})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Capture(context.Background(), []json.RawMessage{raw}); err != nil {
		t.Fatal(err)
	}
	if worked, err := a.ProcessInbox(context.Background()); err != nil || !worked {
		t.Fatalf("process inbox: %v %v", worked, err)
	}
}
func message(actor int64, text string) *telegram.Message {
	return &telegram.Message{ID: 1, Chat: telegram.Chat{ID: -100, Type: "supergroup"}, From: &telegram.User{ID: actor}, Text: text, Date: time.Now().Unix()}
}

func TestGroupAdminCanOverrideButCannotChangeSharedPolicy(t *testing.T) {
	a, _ := appFixture(t, "administrator")
	deliver(t, a, 1, message(42, `/as_set rules.repeat.duration_seconds 120`))
	if a.Config.Snapshot().ForGroup(-100).Rules["repeat"].DurationSeconds != 120 {
		t.Fatal("group override failed")
	}
	if a.Config.Snapshot().ForGroup(-200).Rules["repeat"].DurationSeconds != 600 {
		t.Fatal("group command changed another chat")
	}
	deliver(t, a, 2, message(42, `/as_set base mode "enforce"`))
	if a.Config.Snapshot().Base.Mode != "record" {
		t.Fatal("group admin changed shared policy")
	}
	deliver(t, a, 3, message(42, `/as_reset rules.repeat.duration_seconds`))
	if a.Config.Snapshot().ForGroup(-100).Rules["repeat"].DurationSeconds != 600 {
		t.Fatal("inheritance was not restored")
	}
	deliver(t, a, 4, message(9, `/as_set base queue_limit 7`))
	if a.Config.Snapshot().Base.QueueLimit != 7 {
		t.Fatal("owner could not change shared policy")
	}
}

func TestOrdinaryMembersCannotChangeSettingsOrLabelOthers(t *testing.T) {
	a, path := appFixture(t, "member")
	deliver(t, a, 1, message(42, `/as_set mode "enforce"`))
	if a.Config.Snapshot().ForGroup(-100).Mode != "record" {
		t.Fatal("member escalated permissions")
	}
	m := message(42, "@guard_bot")
	m.Entities = []telegram.Entity{{Type: "mention", Offset: 0, Length: 10}}
	m.Reply = message(50, "ad")
	deliver(t, a, 2, m)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM annotations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("unauthorized report became a training label")
	}
}

func TestReplyMentionRecordsSpamAndHamCanReferenceArchivedMessage(t *testing.T) {
	a, path := appFixture(t, "administrator")
	original := message(50, "ad")
	original.ID = 7
	deliver(t, a, 1, original)
	m := message(42, "😀 @guard_bot")
	m.Entities = []telegram.Entity{{Type: "mention", Offset: 3, Length: 10}}
	m.Reply = original
	deliver(t, a, 2, m)
	m = message(42, "/as_ham 7")
	m.ID = 3
	deliver(t, a, 3, m)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM annotations WHERE chat_id=-100 AND message_id=7 AND actor_id=42`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("lost annotation history: %d", count)
	}
	var raw string
	if err := db.QueryRow(`SELECT target_raw FROM annotations WHERE label='ham'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "ad") {
		t.Fatal("deleted-message correction lost original evidence")
	}
}

func TestAnonymousSenderCannotUseFakeUserForAuthorization(t *testing.T) {
	a, _ := appFixture(t, "administrator")
	m := message(9, `/as_set base mode "enforce"`)
	m.SenderChat = &telegram.Chat{ID: -100, Type: "supergroup"}
	deliver(t, a, 1, m)
	if a.Config.Snapshot().Base.Mode != "record" {
		t.Fatal("anonymous chat acquired owner privileges")
	}
}

func TestOwnerStillNeedsLocalAdministratorRightsForGroupCommands(t *testing.T) {
	a, path := appFixture(t, "member")
	deliver(t, a, 1, message(9, `/as_set mode "enforce"`))
	if a.Config.Snapshot().ForGroup(-100).Mode != "record" {
		t.Fatal("owner changed local policy without group admin rights")
	}
	m := message(9, "@guard_bot")
	m.Entities = []telegram.Entity{{Type: "mention", Offset: 0, Length: 10}}
	m.Reply = message(50, "ad")
	deliver(t, a, 2, m)
	if scalar(t, path, `SELECT count(*) FROM annotations`) != 0 {
		t.Fatal("owner labeled a local message without group admin rights")
	}
	deliver(t, a, 3, message(9, `/as_set base queue_limit 8`))
	if a.Config.Snapshot().Base.QueueLimit != 8 {
		t.Fatal("owner lost shared configuration permissions")
	}
}
