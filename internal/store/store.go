// Package store provides the durable inbox, versioned archive, bounded model
// queue, annotations, and moderation audit trail in one SQLite database.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"telegram-antispam-bot/internal/config"
)

type Store struct {
	db      *sql.DB
	lock    *os.File
	closeMu sync.Mutex
	closed  bool
}
type Inbox struct {
	ID         int64
	Raw        []byte
	ReceivedAt int64
}
type Snapshot struct {
	Policy    config.Policy              `json:"policy"`
	Questions map[string]config.Question `json:"questions"`
}
type Job struct {
	ID         int64
	UpdateID   int64
	ChatID     int64
	MessageID  int
	UserID     int64
	Sender     string
	ReceivedAt int64
	Raw        []byte
	Snapshot   Snapshot
	State      []byte
	Status     string
}
type Action struct {
	ID         int64
	RevisionID int64
	ChatID     int64
	MessageID  int
	UserID     int64
	RuleID     string
	Rule       config.Rule
	Kind       string
	Until      int64
}

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS updates(update_id INTEGER PRIMARY KEY,raw TEXT NOT NULL,received_at INTEGER NOT NULL,processed INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS inbox_pending ON updates(processed,update_id);
CREATE TABLE IF NOT EXISTS revisions(
 id INTEGER PRIMARY KEY,update_id INTEGER NOT NULL UNIQUE REFERENCES updates(update_id),
 chat_id INTEGER NOT NULL,message_id INTEGER NOT NULL,user_id INTEGER NOT NULL,sender TEXT NOT NULL,
 received_at INTEGER NOT NULL,raw TEXT NOT NULL,snapshot TEXT NOT NULL,state TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL CHECK(status IN ('pending','running','succeeded','evicted','superseded','command','ignored')),
 available_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS queue_pending ON revisions(status,available_at,id);
CREATE INDEX IF NOT EXISTS message_versions ON revisions(chat_id,message_id,id);
CREATE TABLE IF NOT EXISTS attempts(
 id INTEGER PRIMARY KEY,revision_id INTEGER NOT NULL REFERENCES revisions(id),created_at INTEGER NOT NULL,
 request TEXT NOT NULL,response TEXT NOT NULL,probabilities TEXT NOT NULL,error TEXT NOT NULL,duration_ms INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS evaluations(
 attempt_id INTEGER PRIMARY KEY REFERENCES attempts(id),result TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS hits(
 chat_id INTEGER NOT NULL,message_id INTEGER NOT NULL,sender TEXT NOT NULL,rule_id TEXT NOT NULL,
 rule_key TEXT NOT NULL,mode TEXT NOT NULL,occurred_at INTEGER NOT NULL,
 PRIMARY KEY(chat_id,message_id,rule_id,rule_key,mode));
CREATE INDEX IF NOT EXISTS hit_window ON hits(chat_id,sender,rule_id,rule_key,mode,occurred_at);
CREATE TABLE IF NOT EXISTS actions(
 id INTEGER PRIMARY KEY,revision_id INTEGER NOT NULL REFERENCES revisions(id),chat_id INTEGER NOT NULL,
 message_id INTEGER NOT NULL,user_id INTEGER NOT NULL,rule_id TEXT NOT NULL,rule TEXT NOT NULL,
 kind TEXT NOT NULL,mode TEXT NOT NULL,until_date INTEGER NOT NULL,status TEXT NOT NULL,
 detail TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,
 UNIQUE(revision_id,kind));
CREATE INDEX IF NOT EXISTS action_pending ON actions(status,id);
CREATE TABLE IF NOT EXISTS annotations(
 report_update_id INTEGER PRIMARY KEY,chat_id INTEGER NOT NULL,message_id INTEGER NOT NULL,
 actor_id INTEGER NOT NULL,label TEXT NOT NULL CHECK(label IN ('spam','ham')),
 target_raw TEXT NOT NULL,created_at INTEGER NOT NULL);
PRAGMA user_version=2;`

func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	lock, err := lockDatabase(abs + ".lock")
	if err != nil {
		return nil, err
	}
	opened := false
	defer func() {
		if !opened {
			lock.Close()
		}
	}()
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(abs, 0600); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	db, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, lock: lock}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version > 2 {
		db.Close()
		return nil, fmt.Errorf("unsupported database schema version %d", version)
	}
	// The additive v2 migration preserves v1 attempts and all pending work.
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err := tx.Exec(schema); err != nil {
		tx.Rollback()
		db.Close()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		return nil, err
	}
	// Model calls can be retried. A moderation request interrupted after dispatch
	// has an unknown remote outcome and is kept for inspection rather than replay.
	if _, err := db.Exec(`UPDATE revisions SET status='pending' WHERE status='running'; UPDATE actions SET status='uncertain',detail='process stopped during dispatch' WHERE status='executing'`); err != nil {
		db.Close()
		return nil, err
	}
	opened = true
	return s, nil
}
func (s *Store) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	// database/sql can finish context-triggered rollbacks after a caller returns.
	// Leasing our sole connection is a barrier for that work before DB.Close.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("drain database before closing: %w", err)
	}
	if err := conn.Close(); err != nil {
		return err
	}
	if err := s.db.Close(); err != nil {
		return err
	}
	s.closed = true
	return s.lock.Close()
}

func (s *Store) Capture(ctx context.Context, raws []json.RawMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, raw := range raws {
		var header struct {
			ID int64 `json:"update_id"`
		}
		if err := json.Unmarshal(raw, &header); err != nil || header.ID <= 0 {
			return errors.New("update has no valid update_id")
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO updates(update_id,raw,received_at) VALUES(?,?,?)`, header.ID, string(raw), time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) LastUpdateID(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(max(update_id),0) FROM updates`).Scan(&id)
	return id, err
}
func (s *Store) NextInbox(ctx context.Context) (*Inbox, error) {
	var in Inbox
	err := s.db.QueryRowContext(ctx, `SELECT update_id,raw,received_at FROM updates WHERE processed=0 ORDER BY update_id LIMIT 1`).Scan(&in.ID, &in.Raw, &in.ReceivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &in, err
}
func (s *Store) MarkInbox(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE updates SET processed=1,error=? WHERE update_id=?`, reason, id)
	return err
}

type scanner interface{ Scan(...any) error }

const jobColumns = `id,update_id,chat_id,message_id,user_id,sender,received_at,raw,snapshot,state,status`

func scanJob(row scanner) (*Job, error) {
	var j Job
	var snapshot []byte
	err := row.Scan(&j.ID, &j.UpdateID, &j.ChatID, &j.MessageID, &j.UserID, &j.Sender, &j.ReceivedAt, &j.Raw, &snapshot, &j.State, &j.Status)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(snapshot, &j.Snapshot); err != nil {
		return nil, err
	}
	return &j, nil
}
func (s *Store) Revision(ctx context.Context, id int64) (*Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM revisions WHERE id=?`, id))
}
func (s *Store) SetState(ctx context.Context, id int64, state []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE revisions SET state=? WHERE id=? AND state=''`, string(state), id)
	return err
}
func (s *Store) Stats(ctx context.Context, chat int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status,count(*) FROM revisions WHERE chat_id=? GROUP BY status`, chat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		stats[status] = count
	}
	return stats, rows.Err()
}
func (s *Store) Label(ctx context.Context, report, chat int64, message int, actor int64, label string, raw []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO annotations(report_update_id,chat_id,message_id,actor_id,label,target_raw,created_at) VALUES(?,?,?,?,?,?,?)`, report, chat, message, actor, label, string(raw), time.Now().UnixMilli())
	return err
}

func (s *Store) LatestMessage(ctx context.Context, chat int64, message int) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT raw FROM revisions WHERE chat_id=? AND message_id=? ORDER BY id DESC LIMIT 1`, chat, message).Scan(&raw)
	return raw, err
}
