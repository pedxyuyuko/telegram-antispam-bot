package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"telegram-antispam-bot/internal/config"
)

func TestMembershipArrivalUsesAffectedUserAndPresentTransition(t *testing.T) {
	for _, tc := range []struct {
		old, next string
		arrival   bool
	}{
		{`"status":"left"`, `"status":"member"`, true},
		{`"status":"kicked"`, `"status":"restricted","is_member":true`, true},
		{`"status":"restricted","is_member":false`, `"status":"member"`, true},
		{`"status":"member"`, `"status":"administrator"`, false},
		{`"status":"restricted","is_member":true`, `"status":"member"`, false},
		{`"status":"member"`, `"status":"left"`, false},
	} {
		raw := `{"update_id":1,"chat_member":{"chat":{"id":-100,"type":"supergroup"},"from":{"id":99,"first_name":"Inviter"},"date":123,"old_chat_member":{` + tc.old + `,"user":{"id":50,"first_name":"Old"}},"new_chat_member":{` + tc.next + `,"user":{"id":50,"first_name":"New","last_name":"Member"}}}}`
		u, err := Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if u.ChatMember == nil || !u.ChatMember.Valid() || u.ChatMember.Joined() != tc.arrival || u.ChatMember.New.User.ID != 50 {
			t.Fatalf("wrong membership interpretation: %+v", u.ChatMember)
		}
	}
	u, err := Parse([]byte(`{"update_id":2,"chat_member":{"chat":{"id":-100,"type":"supergroup"},"date":1,"old_chat_member":{"status":"left","user":{"id":9}},"new_chat_member":{"status":"member","user":{"id":50}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if u.ChatMember.Valid() {
		t.Fatal("mismatching member IDs accepted")
	}
}

func TestJoinProfileQueriesMemberIDAndArchivesMissingBiography(t *testing.T) {
	for _, tc := range []struct{ name, response, status, bio string }{
		{"bio", `{"ok":true,"result":{"id":50,"type":"private","bio":"member bio"}}`, "available", "member bio"},
		{"empty", `{"ok":true,"result":{"id":50,"type":"private"}}`, "available", ""},
		{"wrong_identity", `{"ok":true,"result":{"id":99,"type":"private","bio":"inviter bio"}}`, "unavailable", ""},
		{"failed", `{"ok":false,"error_code":400,"description":"123:secret chat not found"}`, "unavailable", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.ParseMultipartForm(1 << 20)
				if !strings.HasSuffix(r.URL.Path, "/getChat") || r.FormValue("chat_id") != "50" {
					t.Errorf("wrong profile target: %v", r.Form)
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
			p := c.JoinProfile(context.Background(), 50)
			if p.Status != tc.status || p.Bio != tc.bio || p.FetchedAt <= 0 || len(p.Request) == 0 || strings.Contains(p.Error, "123:secret") {
				t.Fatalf("profile=%+v", p)
			}
			state, err := JoinModelState(User{ID: 50, FirstName: "New", LastName: "Member"}, p)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Member struct {
					Name   string
					Bio    *string
					Status string `json:"bio_status"`
				}
			}
			if err := json.Unmarshal(state, &got); err != nil {
				t.Fatal(err)
			}
			if got.Member.Name != "New Member" || got.Member.Status != tc.status {
				t.Fatalf("state=%s", state)
			}
			if tc.bio == "" && got.Member.Bio != nil {
				t.Fatal("missing biography became text")
			}
			if tc.bio != "" && (got.Member.Bio == nil || *got.Member.Bio != tc.bio) {
				t.Fatal("biography lost")
			}
		})
	}
}
