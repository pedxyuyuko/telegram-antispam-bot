package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/telegram"
	"telegram-antispam-bot/internal/vision"
)

const imageSchema = `
CREATE TABLE IF NOT EXISTS revision_images(
 revision_id INTEGER PRIMARY KEY REFERENCES revisions(id),assets TEXT NOT NULL,lookups TEXT NOT NULL,created_at INTEGER NOT NULL);
PRAGMA user_version=10;`

func scanImageAssets(row scanner) (vision.Assets, error) {
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var assets vision.Assets
	err := json.Unmarshal(raw, &assets)
	return assets, err
}

func (s *Store) ImageAssets(ctx context.Context, id int64) (vision.Assets, error) {
	return scanImageAssets(s.db.QueryRowContext(ctx, `SELECT assets FROM revision_images WHERE revision_id=?`, id))
}

// FreezeVisualContext selects state, references, normalized assets and download
// audit in one transaction. A concurrent or historical freeze wins as a whole.
func (s *Store) FreezeVisualContext(ctx context.Context, id int64, state []byte, assets vision.Assets, lookups []telegram.ImageLookup) ([]byte, vision.Assets, error) {
	rawAssets, err := json.Marshal(assets)
	if err != nil {
		return nil, nil, err
	}
	audit, err := json.Marshal(lookups)
	if err != nil {
		return nil, nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE revisions SET state=? WHERE id=? AND state=''`, string(state), id)
	if err != nil {
		return nil, nil, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return nil, nil, err
	}
	if n > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO revision_images(revision_id,assets,lookups,created_at) VALUES(?,?,?,?)`, id, string(rawAssets), string(audit), time.Now().UnixMilli()); err != nil {
			return nil, nil, err
		}
	}
	var saved []byte
	if err := tx.QueryRowContext(ctx, `SELECT state FROM revisions WHERE id=?`, id).Scan(&saved); err != nil {
		return nil, nil, err
	}
	selected, err := scanImageAssets(tx.QueryRowContext(ctx, `SELECT assets FROM revision_images WHERE revision_id=?`, id))
	if err != nil {
		return nil, nil, err
	}
	return saved, selected, tx.Commit()
}

// RecordModelFailure archives the rejected visual request independently, before
// a text-only attempt. It changes neither queue state nor global model backoff.
func (s *Store) RecordModelFailure(ctx context.Context, id int64, result decision.Result, failure error, now time.Time) (int64, error) {
	if failure == nil {
		return 0, errors.New("model failure audit requires an error")
	}
	probabilities, _ := json.Marshal(result.Probabilities)
	r, err := s.db.ExecContext(ctx, `INSERT INTO attempts(revision_id,created_at,request,response,probabilities,error,duration_ms) VALUES(?,?,?,?,?,?,?)`, id, now.UnixMilli(), string(result.Request), string(result.Response), string(probabilities), failure.Error(), result.Duration.Milliseconds())
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}
