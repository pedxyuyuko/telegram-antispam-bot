package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func pageFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/tme/" + name + ".html")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParsePublicPageReadsProfileKinds(t *testing.T) {
	for _, tc := range []struct {
		fixture, kind, title, description, audience string
	}{
		{"bot", "bot", "Group Help", "发财新項木 小白可做 日入5000 + 进群了解 https://t.me/+qSxg1c_Apeo1NGZl", ""},
		{"user", "user", "Alice", "发财新項木 小白可做 日入5000 + 进群了解 https://t.me/+qSxg1c_Apeo1NGZl", ""},
		// Telegram fills empty descriptions with a generic sentence.
		{"default-bot", "bot", "Example Bot", "", ""},
		{"channel", "channel", "Pavel Durov", "Founder of Telegram.", "9 937 027 subscribers"},
		{"group", "group", "大秦帝国电子越共发情之⑨吧", "", "474 members, 12 online"},
		// Line breaks of the visible description survive; og:description has tabs.
		{"invite", "group", "U享社 · 搬砖USDT赚钱交流群", "🏆专注搬砖5年老平台🏆\n🥇专注高效！稳定！安全🥇\n✅小白轻松上手的搬砖项目✅\n👍🏻专人指导，一对一服务，汇率美丽👍🏻", "35 430 members, 609 online"},
	} {
		page, err := parsePublicPage(strings.NewReader(string(pageFixture(t, tc.fixture))))
		if err != nil {
			t.Fatalf("%s: %v", tc.fixture, err)
		}
		if !page.Found || page.Kind != tc.kind || page.Title != tc.title || page.Description != tc.description || page.Audience != tc.audience {
			t.Fatalf("%s: page=%+v", tc.fixture, page)
		}
	}
}

func TestParsePublicPageRecognizesMissingProfile(t *testing.T) {
	page, err := parsePublicPage(strings.NewReader(string(pageFixture(t, "missing"))))
	if err != nil || page.Found || page.Title != "" || page.Description != "" {
		t.Fatalf("missing username produced profile: %+v %v", page, err)
	}
}

func pageClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	c := imageClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("Bot API called for public page") })
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	c.PublicPageBase = s.URL
	return c
}

func TestPublicPageFetchesUsernameAndInvitePaths(t *testing.T) {
	var paths []string
	c := pageClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(pageFixture(t, "invite"))
	})
	for _, value := range []string{"@SomeBot", "https://t.me/+qSxg1c_Apeo1NGZl", "https://telegram.me/joinchat/AbCd_ef"} {
		ref, ok := ParseReference(value)
		if !ok {
			t.Fatal(value)
		}
		page := c.PublicPage(context.Background(), ref)
		if !page.Found || page.HTTPStatus != 200 || page.Title == "" {
			t.Fatalf("%s: %+v", value, page)
		}
	}
	if strings.Join(paths, " ") != "/somebot /+qSxg1c_Apeo1NGZl /joinchat/AbCd_ef" {
		t.Fatalf("paths=%v", paths)
	}
}

func TestPublicPageRefusesRedirectsOversizeAndErrors(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer other.Close()
	for name, handler := range map[string]http.HandlerFunc{
		"redirect": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) },
		"status":   func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
		"oversize": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(strings.Repeat("<p>x</p>", maxPublicPageBytes)))
		},
		"not html": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write(pageFixture(t, "bot"))
		},
	} {
		c := pageClient(t, handler)
		page := c.PublicPage(context.Background(), Reference{Key: "username:x", Username: "x"})
		if page.Found || page.Error == "" {
			t.Fatalf("%s accepted: %+v", name, page)
		}
	}
	if leaked.Load() != 0 {
		t.Fatal("redirect followed")
	}
}

func TestPublicPageCompletesOnlyMissingProfileMaterial(t *testing.T) {
	botPage := PublicPage{Found: true, Kind: "bot", Title: "Group Help", Description: "日入5000 进群了解"}
	// Bot API identity is kept; the page adds the description it cannot read.
	l := ReferenceLookup{Status: "available", Profile: ReferenceProfile{Username: "adbot", Name: "Group Help", Kind: "private"}}
	l.MergePublicPage(botPage)
	if l.Profile.Bio != "日入5000 进群了解" || l.Profile.Kind != "bot" || l.Profile.Name != "Group Help" || l.Page == nil {
		t.Fatalf("lookup=%+v", l)
	}
	// A page whose display name differs is archived but not trusted.
	l = ReferenceLookup{Status: "available", Profile: ReferenceProfile{Username: "adbot", Name: "Other", Kind: "private"}}
	l.MergePublicPage(botPage)
	if l.Profile.Bio != "" || l.Page == nil || l.Page.Error == "" {
		t.Fatalf("mismatched page trusted: %+v", l)
	}
	// Bot API could not resolve the username: the page is the profile.
	l = ReferenceLookup{Status: "unavailable", Error: "chat not found", Reference: Reference{Username: "alice"}}
	l.MergePublicPage(PublicPage{Found: true, Kind: "user", Title: "Alice", Description: "hello"})
	if l.Status != "available" || l.Profile != (ReferenceProfile{Username: "alice", Name: "Alice", Bio: "hello", Kind: "user"}) {
		t.Fatalf("lookup=%+v", l)
	}
	// Invite links become group profiles with their audience size.
	l = ReferenceLookup{Status: "unsupported", Reference: Reference{Key: "invite:https://t.me/+x"}}
	l.MergePublicPage(PublicPage{Found: true, Kind: "group", Title: "U享社", Description: "搬砖", Audience: "35 430 members"})
	if l.Status != "available" || l.Profile != (ReferenceProfile{Name: "U享社", Bio: "搬砖", Kind: "group", Audience: "35 430 members"}) {
		t.Fatalf("lookup=%+v", l)
	}
	// Missing pages leave the original outcome untouched.
	l = ReferenceLookup{Status: "unavailable", Error: "chat not found"}
	l.MergePublicPage(PublicPage{Error: "public page has no profile"})
	if l.Status != "unavailable" || l.Page == nil {
		t.Fatalf("lookup=%+v", l)
	}
	raw, _ := json.Marshal(l)
	if !strings.Contains(string(raw), `"page"`) {
		t.Fatal("page audit not archived")
	}
}
