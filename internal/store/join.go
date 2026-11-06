package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/telegram"
)

const joinSchema = `
CREATE TABLE IF NOT EXISTS join_events(
 id INTEGER PRIMARY KEY,update_id INTEGER NOT NULL UNIQUE REFERENCES updates(update_id),
 chat_id INTEGER NOT NULL,user_id INTEGER NOT NULL,event_at INTEGER NOT NULL,received_at INTEGER NOT NULL,
 is_join INTEGER NOT NULL,present INTEGER NOT NULL,role TEXT NOT NULL,raw TEXT NOT NULL,snapshot TEXT NOT NULL,
 profile TEXT NOT NULL DEFAULT '',state TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL CHECK(status IN ('pending','running','succeeded','evicted','superseded','ignored','disabled','cancelled')),
 detail TEXT NOT NULL DEFAULT '',available_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS join_queue ON join_events(status,available_at,id);
CREATE INDEX IF NOT EXISTS join_member_history ON join_events(chat_id,user_id,event_at,update_id);
CREATE TABLE IF NOT EXISTS join_attempts(
 id INTEGER PRIMARY KEY,join_id INTEGER NOT NULL REFERENCES join_events(id),created_at INTEGER NOT NULL,
 request TEXT NOT NULL,response TEXT NOT NULL,answers TEXT NOT NULL,evaluation TEXT NOT NULL,error TEXT NOT NULL,duration_ms INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS join_actions(
 id INTEGER PRIMARY KEY,join_id INTEGER NOT NULL UNIQUE REFERENCES join_events(id),chat_id INTEGER NOT NULL,user_id INTEGER NOT NULL,
 rule_id TEXT NOT NULL,rule TEXT NOT NULL,mode TEXT NOT NULL,status TEXT NOT NULL,
 detail TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS join_action_pending ON join_actions(status,id);
PRAGMA user_version=8;`

type JoinSnapshot struct {
	Policy        config.JoinPolicy          `json:"policy"`
	Questions     map[string]config.Question `json:"questions"`
	ModelID       string                     `json:"model_id"`
	ModelEndpoint string                     `json:"model_endpoint"`
}

type JoinJob struct {
	ID, UpdateID, ChatID, UserID, ReceivedAt int64
	Raw, State                               []byte
	Snapshot                                 JoinSnapshot
	Event                                    telegram.MembershipUpdate
	Status                                   string
}

const joinColumns = `id,update_id,chat_id,user_id,received_at,raw,snapshot,state,status`

func scanJoin(row scanner) (*JoinJob, error) {
	var j JoinJob
	var snap []byte
	if err := row.Scan(&j.ID, &j.UpdateID, &j.ChatID, &j.UserID, &j.ReceivedAt, &j.Raw, &snap, &j.State, &j.Status); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(snap, &j.Snapshot); err != nil {
		return nil, err
	}
	u, err := telegram.Parse(j.Raw)
	if err != nil {
		return nil, err
	}
	if u.ChatMember == nil {
		return nil, errors.New("archived join event has no membership update")
	}
	j.Event = *u.ChatMember
	return &j, nil
}

func (s *Store) JoinEvent(ctx context.Context, id int64) (*JoinJob, error) {
	return scanJoin(s.db.QueryRowContext(ctx, `SELECT `+joinColumns+` FROM join_events WHERE id=?`, id))
}

func trimJoins(ctx context.Context, tx *sql.Tx, chat int64, limit int) (int64, error) {
	r, err := tx.ExecContext(ctx, `UPDATE join_events SET status='evicted',detail='join queue limit' WHERE id IN (SELECT id FROM join_events WHERE chat_id=? AND status IN ('pending','running') ORDER BY id DESC LIMIT -1 OFFSET ?)`, chat, limit)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// AddJoinEvent archives every member transition. Official chat_member updates
// are the single source of arrival episodes; service messages stay in the inbox.
func (s *Store) AddJoinEvent(ctx context.Context, in Inbox, m telegram.MembershipUpdate, snap JoinSnapshot, skip string) (int64, error) {
	if !m.Valid() {
		return 0, errors.New("invalid membership event")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var existing int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM join_events WHERE update_id=?`, in.ID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	status, detail := "ignored", skip
	if m.Joined() {
		status = "pending"
		if !snap.Policy.Enabled {
			status = "disabled"
			detail = "join gate disabled"
		}
		if skip != "" {
			status = "ignored"
		}
	}
	var newer int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM join_events WHERE chat_id=? AND user_id=? AND (event_at>? OR (event_at=? AND update_id>?))`, m.Chat.ID, m.New.User.ID, m.Date, m.Date, in.ID).Scan(&newer); err != nil {
		return 0, err
	}
	if newer > 0 {
		status = "superseded"
		detail = "newer membership event already observed"
	}
	if newer == 0 && (m.Joined() || !m.New.Present() || m.New.Status == "administrator" || m.New.Status == "creator") {
		if _, err := tx.ExecContext(ctx, `UPDATE join_events SET status='superseded',detail='membership episode changed' WHERE chat_id=? AND user_id=? AND status IN ('pending','running')`, m.Chat.ID, m.New.User.ID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE join_actions SET status='skipped',detail='membership episode changed' WHERE chat_id=? AND user_id=? AND status IN ('pending','checking')`, m.Chat.ID, m.New.User.ID); err != nil {
			return 0, err
		}
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, err
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO join_events(update_id,chat_id,user_id,event_at,received_at,is_join,present,role,raw,snapshot,status,detail) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, in.ID, m.Chat.ID, m.New.User.ID, m.Date, in.ReceivedAt, m.Joined(), m.New.Present(), m.New.Status, string(in.Raw), string(raw), status, detail)
	if err != nil {
		return 0, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := trimJoins(ctx, tx, m.Chat.ID, snap.Policy.QueueLimit); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE updates SET processed=1 WHERE update_id=?`, in.ID); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Store) ClaimJoin(ctx context.Context, now time.Time) (*JoinJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var paused int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce((SELECT CAST(value AS INTEGER) FROM meta WHERE key='join_model_retry_at'),0)`).Scan(&paused); err != nil {
		return nil, err
	}
	if now.UnixMilli() < paused {
		return nil, nil
	}
	j, err := scanJoin(tx.QueryRowContext(ctx, `SELECT `+joinColumns+` FROM join_events WHERE status='pending' AND available_at<=? ORDER BY available_at,id LIMIT 1`, now.UnixMilli()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE join_events SET status='running' WHERE id=?`, j.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	j.Status = "running"
	return j, nil
}

func (s *Store) FreezeJoinContext(ctx context.Context, id int64, profile telegram.JoinProfileLookup, state []byte) ([]byte, error) {
	raw, err := json.Marshal(profile)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE join_events SET state=?,profile=? WHERE id=? AND state=''`, string(state), string(raw), id); err != nil {
		return nil, err
	}
	var saved []byte
	if err := tx.QueryRowContext(ctx, `SELECT state FROM join_events WHERE id=?`, id).Scan(&saved); err != nil {
		return nil, err
	}
	return saved, tx.Commit()
}

func (s *Store) IgnoreJoin(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE join_events SET status='ignored',detail=? WHERE id=? AND status='running'`, reason, id)
	return err
}

func cancelJoinTx(ctx context.Context, tx *sql.Tx, chat int64, reason string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE join_events SET status='cancelled',detail=? WHERE chat_id=? AND status IN ('pending','running')`, reason, chat); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE join_actions SET status='skipped',detail=? WHERE chat_id=? AND status IN ('pending','checking')`, reason, chat); err != nil {
		return err
	}
	return cancelJoinServiceTx(ctx, tx, chat, reason)
}

func (s *Store) CancelJoin(ctx context.Context, chat int64, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := cancelJoinTx(ctx, tx, chat, reason); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ResizeJoin(ctx context.Context, cfg config.Config) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT chat_id FROM join_events UNION SELECT DISTINCT chat_id FROM revisions WHERE json_type(snapshot,'$.join')='object'`)
	if err != nil {
		return err
	}
	var chats []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		chats = append(chats, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, chat := range chats {
		p := cfg.ForJoinGroup(chat)
		if !p.Enabled {
			if err := cancelJoinTx(ctx, tx, chat, "join gate disabled"); err != nil {
				return err
			}
		}
		if p.Mode != "enforce" {
			if _, err := tx.ExecContext(ctx, `UPDATE join_actions SET status='skipped',detail='join gate record mode' WHERE chat_id=? AND status IN ('pending','checking')`, chat); err != nil {
				return err
			}
		}
		_, _, cleanupEnabled := joinDeleteRule(p)
		if p.Mode != "enforce" || !cleanupEnabled {
			if err := cancelJoinServiceTx(ctx, tx, chat, "join cleanup disabled or record mode"); err != nil {
				return err
			}
		}
		if _, err := trimJoins(ctx, tx, chat, p.QueueLimit); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) JoinStats(ctx context.Context, chat int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status,count(*) FROM join_events WHERE chat_id=? AND is_join=1 GROUP BY status`, chat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}
