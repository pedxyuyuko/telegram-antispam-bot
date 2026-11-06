package store

import (
	"context"
	"testing"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/telegram"
)

func TestReferenceSnapshotsReuseQueriesAndFreezePerRevision(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := config.Defaults().Base
	for i := int64(1); i <= 3; i++ {
		add(t, s, i, -100, int(i), 50, p)
	}
	ref, _ := telegram.ParseReference("@examplechannel")
	calls := 0
	fetch := func() telegram.ReferenceLookup {
		calls++
		return telegram.ReferenceLookup{Reference: ref, Status: "available", Profile: telegram.ReferenceProfile{Name: "name", Username: ref.Username, Bio: "bio"}, FetchedAt: time.Now().UnixMilli(), Response: []byte(`{"id":-100,"title":"name"}`)}
	}
	first, err := s.ResolveReference(ctx, 1, 123, ref, 0, 300, fetch)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ResolveReference(ctx, 2, 123, ref, 0, 300, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first.ID != second.ID || !second.Cached {
		t.Fatal("reference cache missed")
	}
	if _, err := s.ResolveReference(ctx, 1, 123, ref, 0, 0, fetch); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("same revision refreshed frozen reference")
	}
	if _, err := s.ResolveReference(ctx, 3, 123, ref, 0, 0, fetch); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("zero TTL failed to refresh new revision")
	}
	var lookups, links int
	s.db.QueryRow(`SELECT count(*) FROM reference_profiles`).Scan(&lookups)
	s.db.QueryRow(`SELECT count(*) FROM revision_reference_profiles`).Scan(&links)
	if lookups != 2 || links != 3 {
		t.Fatalf("lookups=%d links=%d", lookups, links)
	}
}

func TestReferenceFailuresAreArchivedOutsideModelAttempts(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	add(t, s, 1, -100, 1, 50, config.Defaults().Base)
	ref, _ := telegram.ParseReference("https://t.me/+invite")
	result, err := s.ResolveReference(ctx, 1, 123, ref, 0, 300, func() telegram.ReferenceLookup {
		return telegram.ReferenceLookup{Reference: ref, Status: "unsupported", Error: "invite requires a different lookup method", FetchedAt: time.Now().UnixMilli()}
	})
	if err != nil || result.Lookup.Status != "unsupported" {
		t.Fatalf("failure audit=%+v %v", result, err)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM attempts`).Scan(&count)
	if count != 0 {
		t.Fatal("profile query fabricated model attempt")
	}
}

func TestFreezeStateReturnsAlreadyPersistedMaterial(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id := add(t, s, 1, -100, 1, 50, config.Defaults().Base)
	if err := s.SetState(ctx, id, []byte(`{"old":"snapshot"}`)); err != nil {
		t.Fatal(err)
	}
	state, err := s.FreezeState(ctx, id, []byte(`{"new":"candidate"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(state) != `{"old":"snapshot"}` {
		t.Fatalf("used uncommitted material: %s", state)
	}
}

func TestPerMessageReferenceLimitDoesNotBecomeGlobalFailureCache(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i := int64(1); i <= 2; i++ {
		add(t, s, i, -100, int(i), 50, config.Defaults().Base)
	}
	ref, _ := telegram.ParseReference("@examplechannel")
	if _, err := s.ResolveReference(ctx, 1, 123, ref, 0, 300, func() telegram.ReferenceLookup {
		return telegram.ReferenceLookup{Status: "limit", FetchedAt: time.Now().UnixMilli()}
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	result, err := s.ResolveReference(ctx, 2, 123, ref, 0, 300, func() telegram.ReferenceLookup {
		calls++
		return telegram.ReferenceLookup{Status: "available", FetchedAt: time.Now().UnixMilli()}
	})
	if err != nil || calls != 1 || result.Lookup.Status != "available" {
		t.Fatalf("limit contaminated next message: %+v calls=%d err=%v", result, calls, err)
	}
}

func TestExpiredFailedLookupDoesNotResurrectOlderProfile(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ref, _ := telegram.ParseReference("@examplechannel")
	for i := int64(1); i <= 3; i++ {
		add(t, s, i, -100, int(i), 50, config.Defaults().Base)
	}
	for i, status := range []string{"available", "unavailable"} {
		age := 120
		if i == 1 {
			age = 70
		}
		if _, err := s.ResolveReference(ctx, int64(i+1), 123, ref, 0, 0, func() telegram.ReferenceLookup {
			return telegram.ReferenceLookup{Status: status, FetchedAt: time.Now().Add(-time.Duration(age) * time.Second).UnixMilli()}
		}); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	if _, err := s.ResolveReference(ctx, 3, 123, ref, 0, 300, func() telegram.ReferenceLookup {
		calls++
		return telegram.ReferenceLookup{Status: "available", FetchedAt: time.Now().UnixMilli()}
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("older profile superseded a more recent failed lookup")
	}
}
