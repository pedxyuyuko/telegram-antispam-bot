package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"telegram-antispam-bot/internal/telegram"
)

const referenceProfilesSchema = `
CREATE TABLE IF NOT EXISTS reference_profiles(
 id INTEGER PRIMARY KEY,bot_id INTEGER NOT NULL,reference_key TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('available','unavailable','unsupported','limit')),
 fetched_at INTEGER NOT NULL,payload TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS reference_profile_cache ON reference_profiles(bot_id,reference_key,fetched_at DESC,id DESC);
CREATE TABLE IF NOT EXISTS revision_reference_profiles(
 revision_id INTEGER NOT NULL REFERENCES revisions(id),reference_key TEXT NOT NULL,
 lookup_id INTEGER NOT NULL REFERENCES reference_profiles(id),depth INTEGER NOT NULL,
 PRIMARY KEY(revision_id,reference_key));
PRAGMA user_version=7;`

type StoredReference struct {
	ID     int64
	Lookup telegram.ReferenceLookup
	Cached bool
}

func scanReference(row scanner) (*StoredReference, error) {
	var result StoredReference
	var raw []byte
	if err := row.Scan(&result.ID, &raw); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &result.Lookup); err != nil {
		return nil, err
	}
	return &result, nil
}

// ResolveReference keeps network work outside database transactions, preserves
// each lookup outcome, and freezes the first selected snapshot per revision.
func (s *Store) ResolveReference(ctx context.Context, revision, botID int64, ref telegram.Reference, depth, ttlSeconds int, fetch func() telegram.ReferenceLookup) (StoredReference, error) {
	const selected = `SELECT p.id,p.payload FROM revision_reference_profiles r JOIN reference_profiles p ON p.id=r.lookup_id WHERE r.revision_id=? AND r.reference_key=?`
	existing, err := scanReference(s.db.QueryRowContext(ctx, selected, revision, ref.Key))
	if err == nil {
		existing.Cached = true
		return *existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return StoredReference{}, err
	}
	var cached *StoredReference
	now := time.Now().UnixMilli()
	ttl := int64(min(max(ttlSeconds, 0), 3600)) * 1000
	if ttl > 0 {
		cached, err = scanReference(s.db.QueryRowContext(ctx, `SELECT id,payload FROM reference_profiles WHERE bot_id=? AND reference_key=? AND status<>'limit' AND fetched_at<=? AND fetched_at>=? AND (status='available' OR fetched_at>=?) ORDER BY fetched_at DESC,id DESC LIMIT 1`, botID, ref.Key, now, now-ttl, now-min(ttl, 60000)))
		if errors.Is(err, sql.ErrNoRows) {
			cached = nil
		} else if err != nil {
			return StoredReference{}, err
		}
	}
	var lookup telegram.ReferenceLookup
	if cached == nil {
		lookup = fetch()
		lookup.Reference = ref
	} else {
		lookup = cached.Lookup
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(persist, nil)
	if err != nil {
		return StoredReference{}, err
	}
	defer tx.Rollback()
	result := StoredReference{Lookup: lookup, Cached: cached != nil}
	if cached != nil {
		result.ID = cached.ID
	} else {
		raw, err := json.Marshal(lookup)
		if err != nil {
			return StoredReference{}, err
		}
		inserted, err := tx.ExecContext(persist, `INSERT INTO reference_profiles(bot_id,reference_key,status,fetched_at,payload) VALUES(?,?,?,?,?)`, botID, ref.Key, lookup.Status, lookup.FetchedAt, string(raw))
		if err != nil {
			return StoredReference{}, err
		}
		result.ID, err = inserted.LastInsertId()
		if err != nil {
			return StoredReference{}, err
		}
	}
	if _, err := tx.ExecContext(persist, `INSERT OR IGNORE INTO revision_reference_profiles(revision_id,reference_key,lookup_id,depth) VALUES(?,?,?,?)`, revision, ref.Key, result.ID, depth); err != nil {
		return StoredReference{}, err
	}
	chosen, err := scanReference(tx.QueryRowContext(persist, selected, revision, ref.Key))
	if err != nil {
		return StoredReference{}, err
	}
	chosen.Cached = result.Cached || chosen.ID != result.ID
	if err := tx.Commit(); err != nil {
		return StoredReference{}, err
	}
	return *chosen, nil
}
