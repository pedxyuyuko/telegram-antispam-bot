package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"telegram-antispam-bot/internal/config"
)

func TestStateRetainsHiddenLinksButtonsInlineBotAndUnknownFields(t *testing.T) {
	raw := []byte(`{"update_id":11,"message":{"message_id":2,"chat":{"id":-100,"type":"supergroup"},"from":{"id":42,"first_name":"Alice","username":"alice"},"text":"😀点这里","entities":[{"type":"text_link","offset":2,"length":3,"url":"https://example.test/scam"}],"via_bot":{"id":5,"username":"inline_bot"},"reply_markup":{"inline_keyboard":[[{"text":"领取","url":"https://example.test/button"}]]},"future_content":{"ad":"keep this"}}}`)
	u, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Message.State(Profile{Status: "available", Bio: "profile promotion"})
	for _, want := range []string{"Alice", "profile promotion", "https://example.test/scam", "https://example.test/button", "inline_bot", "future_content", "keep this", `"text":"点这里"`} {
		if !strings.Contains(string(state), want) {
			t.Errorf("lost %q in %s", want, state)
		}
	}
	if u.Message.SenderKey() != "user:42" {
		t.Fatal("wrong sender")
	}
}

func TestChannelSenderDoesNotBecomeFakeUser(t *testing.T) {
	u, err := Parse([]byte(`{"update_id":12,"edited_message":{"message_id":2,"chat":{"id":-100,"type":"supergroup"},"sender_chat":{"id":-200,"title":"channel"},"from":{"id":1087968824,"first_name":"fake"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if u.Current().UserID() != 0 || u.Current().SenderKey() != "chat:-200" {
		t.Fatal("channel identity was attributed to fake user")
	}
}

func TestCapturePersistsRawResponseBeforeSDKCanAcknowledge(t *testing.T) {
	response := `{"ok":true,"result":[{"update_id":11,"message":{"unknown":"preserve"}}]}`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(response)) }))
	defer s.Close()
	called := false
	c := &DurableClient{HTTP: s.Client(), Capture: func(ctx context.Context, updates []json.RawMessage) error {
		called = true
		if len(updates) != 1 || !strings.Contains(string(updates[0]), "preserve") {
			t.Fatal("raw response lost")
		}
		return nil
	}}
	req, _ := http.NewRequest("POST", s.URL+"/botsecret/getUpdates", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !called {
		t.Fatal("returned before durable capture")
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != response {
		t.Fatal("SDK response changed")
	}
	c.Capture = func(context.Context, []json.RawMessage) error { return errors.New("disk full") }
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("SDK could acknowledge unpersisted updates")
	}
}

func TestSDKUsesFiniteMuteAndRejoinableKick(t *testing.T) {
	var paths []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		if strings.HasSuffix(r.URL.Path, "restrictChatMember") {
			if r.FormValue("until_date") == "0" || r.FormValue("use_independent_chat_permissions") != "true" {
				t.Error("mute can become permanent or implicitly permissive")
			}
			if strings.Contains(r.FormValue("permissions"), "true") {
				t.Error("mute granted sending permissions")
			}
		}
		if strings.HasSuffix(r.URL.Path, "unbanChatMember") && r.FormValue("only_if_banned") == "true" {
			t.Error("member would not be removed")
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer s.Close()
	cfg := config.Defaults().Telegram
	cfg.APIBase = s.URL
	c, err := New(cfg, "123:secret", s.Client(), 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Mute(context.Background(), -100, 42, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := c.Kick(context.Background(), -100, 42); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[1], "unbanChatMember") {
		t.Fatalf("unexpected punishment calls: %v", paths)
	}
	if err := c.Mute(context.Background(), -100, 42, time.Now().Add(10*time.Second)); err == nil {
		t.Fatal("accepted Telegram's permanent-mute range")
	}
}
