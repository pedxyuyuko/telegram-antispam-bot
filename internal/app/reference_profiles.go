package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"telegram-antispam-bot/internal/store"
	"telegram-antispam-bot/internal/telegram"
)

func (a *App) referencedProfiles(ctx context.Context, j store.Job, m *telegram.Message, profile telegram.Profile, diagnostic *slog.Logger) (map[string]telegram.ReferenceProfile, error) {
	type pending struct {
		ref   telegram.Reference
		depth int
	}
	var queue []pending
	seen := map[string]bool{}
	for _, ref := range m.References(profile) {
		queue = append(queue, pending{ref, 0})
		seen[ref.Key] = true
	}
	out := map[string]telegram.ReferenceProfile{}
	budget, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	requests := 0
	for i := 0; i < len(queue); i++ {
		entry := queue[i]
		ref := entry.ref
		var known int64
		if ref.Username != "" {
			identity, err := a.Store.ResolveBot(ctx, ref.Username)
			if err == nil {
				known = identity.ID
			} else if !errors.Is(err, store.ErrUnknownBot) && !errors.Is(err, store.ErrNotBot) {
				return nil, err
			}
		}
		stored, err := a.Store.ResolveReference(ctx, j.ID, a.Telegram.Me.ID, ref, entry.depth, a.Config.Snapshot().Telegram.ProfileCacheSeconds, func() telegram.ReferenceLookup {
			return a.lookupReferenceWithinBudget(budget, ref, known, &requests)
		})
		if err != nil {
			return nil, err
		}
		debug(diagnostic, "reference.lookup", "lookup_id", stored.ID, "status", stored.Lookup.Status, "cached", stored.Cached, "depth", entry.depth)
		if stored.Lookup.Status != "available" {
			continue
		}
		out[ref.Key] = stored.Lookup.Profile
		if entry.depth == 0 && ref.ExpandBio {
			for _, next := range telegram.ReferencesInText(stored.Lookup.Profile.Bio) {
				if !seen[next.Key] {
					seen[next.Key] = true
					queue = append(queue, pending{next, 1})
				}
			}
		}
	}
	return out, nil
}

func (a *App) lookupReferenceWithinBudget(budget context.Context, ref telegram.Reference, known int64, requests *int) telegram.ReferenceLookup {
	spend := func() bool {
		if budget.Err() != nil || *requests >= 8 {
			return false
		}
		*requests += 1
		return true
	}
	exhausted := func(result telegram.ReferenceLookup) telegram.ReferenceLookup {
		result.Reference, result.Status = ref, "limit"
		result.Error = strings.TrimSuffix("per-message reference query budget exhausted: "+result.Error, ": ")
		if result.FetchedAt == 0 {
			result.FetchedAt = time.Now().UnixMilli()
		}
		return result
	}
	var result telegram.ReferenceLookup
	if ref.Username != "" {
		if !spend() {
			return exhausted(telegram.ReferenceLookup{})
		}
	}
	result = a.Telegram.LookupReference(budget, ref, known)
	if result.Status == "unavailable" && budget.Err() != nil {
		return exhausted(result)
	}
	if !a.Config.Snapshot().PublicPages.Enabled || !needsPublicPage(result) {
		return result
	}
	// The t.me page shares the per-message request count and time budget.
	if !spend() {
		return result
	}
	result.MergePublicPage(a.Telegram.PublicPage(budget, ref))
	return result
}

// needsPublicPage selects what the Bot API cannot supply: invite links,
// usernames it cannot resolve, and private accounts without a description.
func needsPublicPage(l telegram.ReferenceLookup) bool {
	switch l.Status {
	case "unsupported":
		return strings.HasPrefix(l.Reference.Key, "invite:")
	case "unavailable":
		return l.Reference.Username != ""
	case "available":
		return l.Profile.Kind == "private" && l.Profile.Bio == ""
	}
	return false
}
