package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"telegram-antispam-bot/internal/telegram"
)

const blacklistSchema = `
CREATE TABLE IF NOT EXISTS bot_blacklist(
 bot_id INTEGER PRIMARY KEY CHECK(bot_id>0),username TEXT NOT NULL DEFAULT '',name TEXT NOT NULL DEFAULT '',
 active INTEGER NOT NULL CHECK(active IN (0,1)),generation INTEGER NOT NULL CHECK(generation>0),
 source TEXT NOT NULL,source_action_id INTEGER REFERENCES actions(id),actor_id INTEGER NOT NULL DEFAULT 0,
 released_through_revision INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS bot_blacklist_events(
 id INTEGER PRIMARY KEY,bot_id INTEGER NOT NULL REFERENCES bot_blacklist(bot_id),kind TEXT NOT NULL,
 actor_id INTEGER NOT NULL DEFAULT 0,command_update_id INTEGER UNIQUE,source_action_id INTEGER REFERENCES actions(id),created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS blacklist_matches(
 revision_id INTEGER PRIMARY KEY REFERENCES revisions(id),bot_id INTEGER NOT NULL REFERENCES bot_blacklist(bot_id),generation INTEGER NOT NULL,created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS action_evidence(
 action_id INTEGER PRIMARY KEY REFERENCES actions(id),kind TEXT NOT NULL CHECK(kind IN ('bot_blacklist','bot_invocation')),
 bot_id INTEGER NOT NULL,generation INTEGER NOT NULL DEFAULT 0,source_action_id INTEGER REFERENCES actions(id));
CREATE TABLE IF NOT EXISTS bot_invocations(
 source_action_id INTEGER PRIMARY KEY REFERENCES actions(id),bot_id INTEGER NOT NULL,chat_id INTEGER NOT NULL,message_id INTEGER NOT NULL,
 user_id INTEGER NOT NULL,fingerprint TEXT NOT NULL,target_action_id INTEGER REFERENCES actions(id),created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS pending_bot_invocations ON bot_invocations(chat_id,message_id) WHERE target_action_id IS NULL;
PRAGMA user_version=3;`

type BotIdentity struct {
	ID             int64
	Username, Name string
}
type BlacklistEntry struct {
	BotIdentity
	Active                                                                 bool
	Generation                                                             int64
	Source                                                                 string
	SourceActionID, ActorID, ReleasedThroughRevision, CreatedAt, UpdatedAt int64
}
type BlacklistChange struct{ Changed, Duplicate bool }

const blacklistColumns = `bot_id,username,name,active,generation,source,coalesce(source_action_id,0),actor_id,released_through_revision,created_at,updated_at`

func scanBlacklist(row scanner) (*BlacklistEntry, error) {
	var b BlacklistEntry
	err := row.Scan(&b.ID, &b.Username, &b.Name, &b.Active, &b.Generation, &b.Source, &b.SourceActionID, &b.ActorID, &b.ReleasedThroughRevision, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}
func (s *Store) BotBlacklist(ctx context.Context, id int64) (*BlacklistEntry, error) {
	return scanBlacklist(s.db.QueryRowContext(ctx, `SELECT `+blacklistColumns+` FROM bot_blacklist WHERE bot_id=?`, id))
}
func blacklistTx(ctx context.Context, tx *sql.Tx, id int64) (*BlacklistEntry, error) {
	return scanBlacklist(tx.QueryRowContext(ctx, `SELECT `+blacklistColumns+` FROM bot_blacklist WHERE bot_id=?`, id))
}

// Manual changes are keyed by the Telegram update, so replaying a command cannot
// undo a later owner decision. Removed rows remain as audit tombstones.
func (s *Store) SetBotBlocked(ctx context.Context, bot BotIdentity, blocked bool, actor, update int64) (BlacklistChange, error) {
	if bot.ID <= 0 || actor <= 0 || update <= 0 {
		return BlacklistChange{}, errors.New("positive bot, actor and update IDs are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BlacklistChange{}, err
	}
	defer tx.Rollback()
	var duplicate int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM bot_blacklist_events WHERE command_update_id=?`, update).Scan(&duplicate); err != nil {
		return BlacklistChange{}, err
	}
	if duplicate > 0 {
		return BlacklistChange{Duplicate: true}, nil
	}
	old, err := blacklistTx(ctx, tx, bot.ID)
	if err != nil {
		return BlacklistChange{}, err
	}
	now := time.Now().UnixMilli()
	generation, created, released := int64(1), now, int64(0)
	changed := blocked
	if old != nil {
		generation, created, released = old.Generation, old.CreatedAt, old.ReleasedThroughRevision
		changed = old.Active != blocked
		if changed {
			generation++
		}
		if bot.Username == "" {
			bot.Username = old.Username
		}
		if bot.Name == "" {
			bot.Name = old.Name
		}
	}
	if !blocked {
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(id),0) FROM revisions`).Scan(&released); err != nil {
			return BlacklistChange{}, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO bot_blacklist(bot_id,username,name,active,generation,source,actor_id,released_through_revision,created_at,updated_at)
 VALUES(?,?,?,?,?,'manual',?,?,?,?) ON CONFLICT(bot_id) DO UPDATE SET username=excluded.username,name=excluded.name,active=excluded.active,generation=excluded.generation,source='manual',source_action_id=NULL,actor_id=excluded.actor_id,released_through_revision=excluded.released_through_revision,updated_at=excluded.updated_at`, bot.ID, bot.Username, bot.Name, blocked, generation, actor, released, created, now)
	if err != nil {
		return BlacklistChange{}, err
	}
	kind := "manual_remove"
	if blocked {
		kind = "manual_add"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO bot_blacklist_events(bot_id,kind,actor_id,command_update_id,created_at) VALUES(?,?,?,?,?)`, bot.ID, kind, actor, update, now); err != nil {
		return BlacklistChange{}, err
	}
	if err := tx.Commit(); err != nil {
		return BlacklistChange{}, err
	}
	return BlacklistChange{Changed: changed}, nil
}

func (s *Store) ListBlockedBots(ctx context.Context, offset, limit int) ([]BlacklistEntry, int, error) {
	if offset < 0 || limit < 1 || limit > 100 {
		return nil, 0, errors.New("invalid blacklist page")
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM bot_blacklist WHERE active=1`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+blacklistColumns+` FROM bot_blacklist WHERE active=1 ORDER BY bot_id LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var entries []BlacklistEntry
	for rows.Next() {
		entry, err := scanBlacklist(rows)
		if err != nil {
			return nil, 0, err
		}
		entries = append(entries, *entry)
	}
	return entries, total, rows.Err()
}

func botIdentity(m *telegram.Message) BotIdentity {
	if m.BotSenderID() == 0 {
		return BotIdentity{}
	}
	return BotIdentity{ID: m.From.ID, Username: m.From.Username, Name: strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)}
}

func learnBotTx(ctx context.Context, tx *sql.Tx, m *telegram.Message, revision, action int64, source string) (bool, error) {
	if !m.IsGroup() || m.BotSenderID() == 0 || m.IsAutomaticForward {
		return false, nil
	}
	bot := botIdentity(m)
	old, err := blacklistTx(ctx, tx, bot.ID)
	if err != nil {
		return false, err
	}
	if old != nil && (old.Active || revision <= old.ReleasedThroughRevision) {
		return false, nil
	}
	now := time.Now().UnixMilli()
	generation, created, released := int64(1), now, int64(0)
	if old != nil {
		generation = old.Generation + 1
		created = old.CreatedAt
		released = old.ReleasedThroughRevision
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO bot_blacklist(bot_id,username,name,active,generation,source,source_action_id,actor_id,released_through_revision,created_at,updated_at)
 VALUES(?,?,?,1,?,?,?,0,?,?,?) ON CONFLICT(bot_id) DO UPDATE SET username=excluded.username,name=excluded.name,active=1,generation=excluded.generation,source=excluded.source,source_action_id=excluded.source_action_id,actor_id=0,updated_at=excluded.updated_at`, bot.ID, bot.Username, bot.Name, generation, source, action, released, created, now)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO bot_blacklist_events(bot_id,kind,source_action_id,created_at) VALUES(?,?,?,?)`, bot.ID, source+"_add", action, now); err != nil {
		return false, err
	}
	return true, nil
}

// Bootstrap imports identities only; historical caller messages and actions stay
// untouched. The marker and tombstones prevent startup from undoing removals.
func bootstrapBotBlacklist(ctx context.Context, tx *sql.Tx) error {
	var initialized int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM meta WHERE key='bot_blacklist_initialized'`).Scan(&initialized); err != nil {
		return err
	}
	if initialized > 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,r.id,r.raw FROM actions a JOIN revisions r ON r.id=a.revision_id
 LEFT JOIN action_evidence e ON e.action_id=a.id
 WHERE a.kind='delete' AND a.mode='enforce' AND a.status='succeeded' AND e.action_id IS NULL
 AND json_extract(r.raw,'$.from.is_bot')=1 AND json_extract(r.raw,'$.sender_chat') IS NULL ORDER BY a.id`)
	if err != nil {
		return err
	}
	type evidence struct {
		action, revision int64
		raw              []byte
	}
	var found []evidence
	for rows.Next() {
		var e evidence
		if err := rows.Scan(&e.action, &e.revision, &e.raw); err != nil {
			rows.Close()
			return err
		}
		found = append(found, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, e := range found {
		var m telegram.Message
		if err := json.Unmarshal(e.raw, &m); err != nil {
			return err
		}
		if _, err := learnBotTx(ctx, tx, &m, e.revision, e.action, "history"); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('bot_blacklist_initialized','1')`)
	return err
}
