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

func trim(ctx context.Context, tx *sql.Tx, chat int64, limit int) (int64, error) {
	result, err := tx.ExecContext(ctx, `UPDATE revisions SET status='evicted' WHERE id IN (
 SELECT id FROM revisions WHERE chat_id=? AND status IN ('pending','running') ORDER BY id DESC LIMIT -1 OFFSET ?
 )`, chat, limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) AddRevision(ctx context.Context, in Inbox, m *telegram.Message, snapshot Snapshot, status string) (ArchiveReport, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ArchiveReport{}, err
	}
	defer tx.Rollback()
	var existing int64
	var existingStatus string
	err = tx.QueryRowContext(ctx, `SELECT id,status FROM revisions WHERE update_id=?`, in.ID).Scan(&existing, &existingStatus)
	if err == nil {
		return ArchiveReport{ID: existing, Status: existingStatus, Duplicate: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ArchiveReport{}, err
	}
	// Every retained version is inferred. Stale results are archived without
	// counting or acting; bounded-queue eviction is the only capacity shortcut.
	rawSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		return ArchiveReport{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO revisions(update_id,chat_id,message_id,user_id,sender,received_at,raw,snapshot,status) VALUES(?,?,?,?,?,?,?,?,?)`, in.ID, m.Chat.ID, m.ID, m.UserID(), m.SenderKey(), in.ReceivedAt, string(m.Bytes()), string(rawSnapshot), status)
	if err != nil {
		return ArchiveReport{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ArchiveReport{}, err
	}
	evicted, err := trim(ctx, tx, m.Chat.ID, snapshot.Policy.QueueLimit)
	if err != nil {
		return ArchiveReport{}, err
	}
	if err := tx.Commit(); err != nil {
		return ArchiveReport{}, err
	}
	return ArchiveReport{ID: id, Status: status, Evicted: evicted}, nil
}

func (s *Store) Resize(ctx context.Context, cfg config.Config) ([]QueueChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT chat_id FROM revisions WHERE status IN ('pending','running') ORDER BY chat_id`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var changes []QueueChange
	for _, id := range ids {
		limit := cfg.ForGroup(id).QueueLimit
		evicted, err := trim(ctx, tx, id, limit)
		if err != nil {
			return nil, err
		}
		changes = append(changes, QueueChange{ChatID: id, Limit: limit, Evicted: evicted})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return changes, nil
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

// IgnoreRevision closes claimed work without creating an inference attempt.
// An in-flight admission check must never resurrect an evicted revision.
func (s *Store) IgnoreRevision(ctx context.Context, id int64, reason string) (bool, error) {
	if reason == "" {
		return false, errors.New("skip reason is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE revisions SET status='ignored',snapshot=json_set(snapshot,'$.skip_reason',?) WHERE id=? AND status='running'`, reason, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}
