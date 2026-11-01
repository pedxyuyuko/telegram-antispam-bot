package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"telegram-antispam-bot/internal/telegram"
)

const blacklistSchema = `
CREATE TABLE IF NOT EXISTS bot_blacklist(
 bot_id INTEGER PRIMARY KEY CHECK(bot_id>0),username TEXT NOT NULL DEFAULT '',name TEXT NOT NULL DEFAULT '',
 active INTEGER NOT NULL CHECK(active IN (0,1)),generation INTEGER NOT NULL CHECK(generation>0),
 source TEXT NOT NULL,source_action_id INTEGER REFERENCES actions(id),actor_id INTEGER NOT NULL DEFAULT 0,
 released_through_revision INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,note TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS bot_blacklist_events(
 id INTEGER PRIMARY KEY,bot_id INTEGER NOT NULL REFERENCES bot_blacklist(bot_id),kind TEXT NOT NULL,
 actor_id INTEGER NOT NULL DEFAULT 0,command_update_id INTEGER UNIQUE,source_action_id INTEGER REFERENCES actions(id),created_at INTEGER NOT NULL,detail TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS blacklist_matches(
 revision_id INTEGER PRIMARY KEY REFERENCES revisions(id),bot_id INTEGER NOT NULL REFERENCES bot_blacklist(bot_id),generation INTEGER NOT NULL,created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS action_evidence(
 action_id INTEGER PRIMARY KEY REFERENCES actions(id),kind TEXT NOT NULL CHECK(kind IN ('bot_blacklist','bot_invocation')),
 bot_id INTEGER NOT NULL,generation INTEGER NOT NULL DEFAULT 0,source_action_id INTEGER REFERENCES actions(id));
CREATE TABLE IF NOT EXISTS bot_action_events(
 id INTEGER PRIMARY KEY,action_id INTEGER NOT NULL REFERENCES actions(id),event TEXT NOT NULL,
 source_action_id INTEGER REFERENCES actions(id),mode TEXT NOT NULL,status TEXT NOT NULL,
 rule_id TEXT NOT NULL,rule TEXT NOT NULL,evidence_kind TEXT NOT NULL,detail TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS bot_invocations(
 source_action_id INTEGER PRIMARY KEY REFERENCES actions(id),bot_id INTEGER NOT NULL,chat_id INTEGER NOT NULL,message_id INTEGER NOT NULL,
 user_id INTEGER NOT NULL,fingerprint TEXT NOT NULL,target_action_id INTEGER REFERENCES actions(id),created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS pending_bot_invocations ON bot_invocations(chat_id,message_id) WHERE target_action_id IS NULL;
CREATE INDEX IF NOT EXISTS bot_invocation_targets ON bot_invocations(chat_id,message_id);
CREATE TABLE IF NOT EXISTS bot_invocation_actions(
 source_action_id INTEGER NOT NULL REFERENCES bot_invocations(source_action_id),
 target_action_id INTEGER NOT NULL REFERENCES actions(id),
 PRIMARY KEY(source_action_id,target_action_id));
CREATE INDEX IF NOT EXISTS invocation_action_sources ON bot_invocation_actions(target_action_id,source_action_id);
INSERT OR IGNORE INTO bot_invocation_actions(source_action_id,target_action_id)
 SELECT source_action_id,target_action_id FROM bot_invocations WHERE target_action_id IS NOT NULL;
PRAGMA user_version=5;`

type BotIdentity struct {
	ID             int64
	Username, Name string
}
type BlacklistEntry struct {
	BotIdentity
	Active                                                                 bool
	Generation                                                             int64
	Source                                                                 string
	Note                                                                   string
	SourceActionID, ActorID, ReleasedThroughRevision, CreatedAt, UpdatedAt int64
}
type BlacklistChange struct{ Changed, Duplicate bool }

var ErrUnknownBot = errors.New("bot username has not been observed")
var ErrNotBot = errors.New("stored identity is not a bot")

// ResolveBot uses captured Telegram identities. Numeric IDs may be registered
// before observation; live blacklist matching still requires from.is_bot=true.
func (s *Store) ResolveBot(ctx context.Context, reference string) (BotIdentity, error) {
	ref := strings.TrimSpace(reference)
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		if id <= 0 {
			return BotIdentity{}, ErrUnknownBot
		}
		var raw []byte
		err = s.db.QueryRowContext(ctx, `SELECT raw FROM revisions WHERE user_id=? ORDER BY id DESC LIMIT 1`, id).Scan(&raw)
		if err == nil {
			var m telegram.Message
			if err := json.Unmarshal(raw, &m); err != nil {
				return BotIdentity{}, err
			}
			if m.BotSenderID() != id {
				return BotIdentity{}, ErrNotBot
			}
			return botIdentity(&m), nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return BotIdentity{}, err
		}
		entry, err := s.BotBlacklist(ctx, id)
		if err != nil {
			return BotIdentity{}, err
		}
		if entry != nil {
			return entry.BotIdentity, nil
		}
		return BotIdentity{ID: id}, nil
	}
	username := strings.ToLower(strings.TrimPrefix(ref, "@"))
	if username == "" || len(strings.Fields(username)) != 1 {
		return BotIdentity{}, ErrUnknownBot
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT r.raw FROM revisions r WHERE r.user_id>0 AND json_extract(r.raw,'$.from.is_bot')=1 AND json_extract(r.raw,'$.sender_chat') IS NULL
 AND lower(json_extract(r.raw,'$.from.username'))=? AND r.id=(SELECT max(x.id) FROM revisions x WHERE x.user_id=r.user_id)
 ORDER BY r.id DESC LIMIT 1`, username).Scan(&raw)
	if err == nil {
		var m telegram.Message
		if err := json.Unmarshal(raw, &m); err != nil {
			return BotIdentity{}, err
		}
		return botIdentity(&m), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return BotIdentity{}, err
	}
	entry, err := scanBlacklist(s.db.QueryRowContext(ctx, `SELECT `+blacklistColumns+` FROM bot_blacklist b WHERE lower(username)=?
 AND NOT EXISTS (SELECT 1 FROM revisions r WHERE r.user_id=b.bot_id AND r.id=(SELECT max(x.id) FROM revisions x WHERE x.user_id=b.bot_id)
 AND (coalesce(json_extract(r.raw,'$.from.is_bot'),0)<>1 OR lower(coalesce(json_extract(r.raw,'$.from.username'),''))<>?))
 ORDER BY updated_at DESC,bot_id LIMIT 1`, username, username))
	if err != nil {
		return BotIdentity{}, err
	}
	if entry == nil {
		return BotIdentity{}, ErrUnknownBot
	}
	return entry.BotIdentity, nil
}

const blacklistColumns = `bot_id,username,name,active,generation,source,coalesce(source_action_id,0),actor_id,released_through_revision,created_at,updated_at,note`

func scanBlacklist(row scanner) (*BlacklistEntry, error) {
	var b BlacklistEntry
	err := row.Scan(&b.ID, &b.Username, &b.Name, &b.Active, &b.Generation, &b.Source, &b.SourceActionID, &b.ActorID, &b.ReleasedThroughRevision, &b.CreatedAt, &b.UpdatedAt, &b.Note)
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
	kind := "manual_remove"
	if blocked {
		kind = "manual_add"
	}
	return s.changeBotBlacklist(ctx, bot, BlacklistEdit{Active: &blocked}, actor, update, true, kind)
}

func (s *Store) ListBlockedBots(ctx context.Context, offset, limit int) ([]BlacklistEntry, int, error) {
	return s.ListBotBlacklist(ctx, "active", offset, limit)
}

func (s *Store) ListBotBlacklist(ctx context.Context, status string, offset, limit int) ([]BlacklistEntry, int, error) {
	if offset < 0 || limit < 1 || limit > 100 {
		return nil, 0, errors.New("invalid blacklist page")
	}
	where := ""
	switch status {
	case "active":
		where = "active=1"
	case "inactive":
		where = "active=0"
	case "all":
		where = "1=1"
	default:
		return nil, 0, errors.New("invalid blacklist status")
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM bot_blacklist WHERE `+where).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+blacklistColumns+` FROM bot_blacklist WHERE `+where+` ORDER BY bot_id LIMIT ? OFFSET ?`, limit, offset)
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
