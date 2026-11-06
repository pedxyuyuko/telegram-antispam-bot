package telegram

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"telegram-antispam-bot/internal/config"
)

func TestReferenceLookupUsesGetChatAndPreservesParsedResponse(t *testing.T) {
	for _, tc := range []struct {
		name, reference, query, response, status string
		id                                       int64
	}{
		{"channel", "https://t.me/examplechannel/12", "@examplechannel", `{"ok":true,"result":{"id":-100,"type":"channel","title":"频道","username":"examplechannel","description":"频道简介"}}`, "available", 0},
		{"known_bot", "@adbot", "200", `{"ok":true,"result":{"id":200,"type":"private","first_name":"Bot","username":"adbot","bio":"bot bio"}}`, "available", 200},
		{"renamed_bot", "@adbot", "200", `{"ok":true,"result":{"id":200,"type":"private","first_name":"Bot","username":"renamedbot"}}`, "unavailable", 200},
		{"unavailable", "@unknownbot", "@unknownbot", `{"ok":false,"error_code":400,"description":"123:secret chat not found"}`, "unavailable", 0},
		{"invite", "https://t.me/+PrivateInvite", "", "", "unsupported", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				r.ParseMultipartForm(1 << 20)
				if !strings.HasSuffix(r.URL.Path, "/getChat") || r.FormValue("chat_id") != tc.query {
					t.Errorf("wrong lookup: %s %v", r.URL.Path, r.Form)
				}
				w.Write([]byte(tc.response))
			}))
			defer s.Close()
			cfg := config.Defaults().Telegram
			cfg.APIBase = s.URL
			c, err := New(cfg, "123:secret", s.Client(), 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			ref, ok := ParseReference(tc.reference)
			if !ok {
				t.Fatal("invalid fixture")
			}
			result := c.LookupReference(context.Background(), ref, tc.id)
			if result.Status != tc.status {
				t.Fatalf("lookup=%+v", result)
			}
			if result.FetchedAt <= 0 || strings.Contains(result.Error, "123:secret") {
				t.Fatal("lookup time missing or credential exposed")
			}
			if tc.status == "available" && (result.Profile.Name == "" || len(result.Response) == 0 || result.ChatID == 0) {
				t.Fatal("reference data lost")
			}
			if tc.query == "" && calls != 0 {
				t.Fatal("invite was sent as a chat ID")
			}
		})
	}
}
