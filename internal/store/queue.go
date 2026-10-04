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

func trim(ctx context.Context, tx *sql.Tx, chat int64, limit int) error {
	_, err := tx.ExecContext(ctx, `UPDATE revisions SET status='evicted' WHERE id IN (
 SELECT id FROM revisions WHERE chat_id=? AND status IN ('pending','running') ORDER BY id DESC LIMIT -1 OFFSET ?
 )`, chat, limit)
	return err
}

func (s *Store) AddRevision(ctx context.Context, in Inbox, m *telegram.Message, snapshot Snapshot, status string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var existing int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM revisions WHERE update_id=?`, in.ID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	// Every edit keeps its own archived version; only the newest version remains
	// eligible for future inference or moderation.
	if _, err := tx.ExecContext(ctx, `UPDATE revisions SET status='superseded' WHERE chat_id=? AND message_id=? AND status IN ('pending','running')`, m.Chat.ID, m.ID); err != nil {
		return 0, err
	}
	rawSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO revisions(update_id,chat_id,message_id,user_id,sender,received_at,raw,snapshot,status) VALUES(?,?,?,?,?,?,?,?,?)`, in.ID, m.Chat.ID, m.ID, m.UserID(), m.SenderKey(), in.ReceivedAt, string(m.Bytes()), string(rawSnapshot), status)
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := trim(ctx, tx, m.Chat.ID, snapshot.Policy.QueueLimit); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) Resize(ctx context.Context, cfg config.Config) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT chat_id FROM revisions WHERE status IN ('pending','running')`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := trim(ctx, tx, id, cfg.ForGroup(id).QueueLimit); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Claim(ctx context.Context, now time.Time) (*Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var pausedUntil int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce((SELECT CAST(value AS INTEGER) FROM meta WHERE key='model_retry_at'),0)`).Scan(&pausedUntil); err != nil {
		return nil, err
	}
	if now.UnixMilli() < pausedUntil {
		return nil, nil
	}
	j, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM revisions WHERE status='pending' AND available_at<=? ORDER BY available_at,id LIMIT 1`, now.UnixMilli()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE revisions SET status='running' WHERE id=?`, j.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	j.Status = "running"
	return j, nil
}
