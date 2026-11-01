package telegram

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"telegram-antispam-bot/internal/config"
)

func TestBanPreservesPermanentAndFiniteIntentAtTelegramBoundary(t *testing.T) {
	for _, tc := range []struct {
		name           string
		until          time.Time
		result         string
		valid, success bool
	}{
		{"permanent", time.Time{}, `{"ok":true,"result":true}`, true, true},
		{"finite", time.Now().Add(time.Hour), `{"ok":true,"result":true}`, true, true},
		{"expired", time.Now().Add(-time.Second), "", false, false},
		{"too_soon", time.Now().Add(20 * time.Second), "", false, false},
		{"too_far", time.Now().Add(367 * 24 * time.Hour), "", false, false},
		{"declined", time.Time{}, `{"ok":true,"result":false}`, true, false},
		{"api_error", time.Time{}, `{"ok":false,"error_code":403,"description":"123:secret forbidden"}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				r.ParseMultipartForm(1 << 20)
				if !strings.HasSuffix(r.URL.Path, "/banChatMember") || r.FormValue("user_id") != "42" || r.FormValue("chat_id") != "-100" {
					t.Errorf("wrong ban target or method: %s %v", r.URL.Path, r.Form)
				}
				until := r.FormValue("until_date")
				if tc.until.IsZero() {
					if until != "" && until != "0" {
						t.Errorf("permanent deadline=%s", until)
					}
				} else if until != strconv.FormatInt(tc.until.Unix(), 10) {
					t.Errorf("finite deadline=%s", until)
				}
				w.Write([]byte(tc.result))
			}))
			defer s.Close()
			cfg := config.Defaults().Telegram
			cfg.APIBase = s.URL
			c, err := New(cfg, "123:secret", s.Client(), 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			err = c.Ban(context.Background(), -100, 42, tc.until)
			if (err == nil) != tc.success {
				t.Fatalf("success=%v err=%v", tc.success, err)
			}
			if err != nil && strings.Contains(err.Error(), "123:secret") {
				t.Fatal("token disclosed")
			}
			if (calls == 1) != tc.valid {
				t.Fatalf("invalid deadline dispatched: calls=%d", calls)
			}
		})
	}
}
