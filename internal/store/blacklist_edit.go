package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"
)

var ErrBlacklistEntryNotFound = errors.New("blacklist entry not found")

type BlacklistEdit struct {
	Note   *string
	Active *bool
}

func (s *Store) EditBotBlacklist(ctx context.Context, id int64, edit BlacklistEdit, actor, update int64) (BlacklistChange, error) {
	return s.changeBotBlacklist(ctx, BotIdentity{ID: id}, edit, actor, update, false, "manual_edit")
}

func (s *Store) changeBotBlacklist(ctx context.Context, bot BotIdentity, edit BlacklistEdit, actor, update int64, create bool, kind string) (BlacklistChange, error) {
	if bot.ID <= 0 || actor <= 0 || update <= 0 {
		return BlacklistChange{}, errors.New("positive bot, actor and update IDs are required")
	}
	if edit.Note == nil && edit.Active == nil {
		return BlacklistChange{}, errors.New("provide note or active")
	}
	if edit.Note != nil && utf8.RuneCountInString(*edit.Note) > 1000 {
		return BlacklistChange{}, errors.New("note exceeds 1000 characters")
	}
	if edit.Active != nil {
		s.botDispatchMu.Lock()
		defer s.botDispatchMu.Unlock()
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
	if old == nil && !create {
		return BlacklistChange{}, ErrBlacklistEntryNotFound
	}
	now := time.Now().UnixMilli()
	generation, created, released := int64(1), now, int64(0)
	active, oldActive := false, false
	note, oldNote := "", ""
	source := "manual"
	sourceActor := actor
	var sourceAction any
	if old != nil {
		generation, created, released = old.Generation, old.CreatedAt, old.ReleasedThroughRevision
		active, oldActive, note, oldNote = old.Active, old.Active, old.Note, old.Note
		source, sourceActor = old.Source, old.ActorID
		if old.SourceActionID > 0 {
			sourceAction = old.SourceActionID
		}
		if bot.Username == "" {
			bot.Username = old.Username
		}
		if bot.Name == "" {
			bot.Name = old.Name
		}
	}
	if edit.Note != nil {
		note = *edit.Note
	}
	if edit.Active != nil {
		active = *edit.Active
		source = "manual"
		sourceActor = actor
		sourceAction = nil
		if old != nil && active != oldActive {
			generation++
		}
		if !active {
			if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(id),0) FROM revisions`).Scan(&released); err != nil {
				return BlacklistChange{}, err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO bot_blacklist(bot_id,username,name,active,generation,source,source_action_id,actor_id,released_through_revision,created_at,updated_at,note)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(bot_id) DO UPDATE SET username=excluded.username,name=excluded.name,active=excluded.active,generation=excluded.generation,source=excluded.source,source_action_id=excluded.source_action_id,actor_id=excluded.actor_id,released_through_revision=excluded.released_through_revision,updated_at=excluded.updated_at,note=excluded.note`, bot.ID, bot.Username, bot.Name, active, generation, source, sourceAction, sourceActor, released, created, now, note)
	if err != nil {
		return BlacklistChange{}, err
	}
	detail, _ := json.Marshal(map[string]any{"before": map[string]any{"active": oldActive, "note": oldNote}, "after": map[string]any{"active": active, "note": note}})
	if _, err := tx.ExecContext(ctx, `INSERT INTO bot_blacklist_events(bot_id,kind,actor_id,command_update_id,created_at,detail) VALUES(?,?,?,?,?,?)`, bot.ID, kind, actor, update, now, string(detail)); err != nil {
		return BlacklistChange{}, err
	}
	if err := tx.Commit(); err != nil {
		return BlacklistChange{}, err
	}
	return BlacklistChange{Changed: active != oldActive || note != oldNote}, nil
}

func migrateBlacklistMetadata(tx *sql.Tx) error {
	for _, change := range []struct{ table, column, ddl string }{
		{"bot_blacklist", "note", `ALTER TABLE bot_blacklist ADD COLUMN note TEXT NOT NULL DEFAULT ''`},
		{"bot_blacklist_events", "detail", `ALTER TABLE bot_blacklist_events ADD COLUMN detail TEXT NOT NULL DEFAULT ''`},
	} {
		rows, err := tx.Query(`PRAGMA table_info(` + change.table + `)`)
		if err != nil {
			return err
		}
		found := false
		for rows.Next() {
			var cid, notnull, pk int
			var name, kind string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
				rows.Close()
				return err
			}
			if name == change.column {
				found = true
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if !found {
			if _, err := tx.Exec(change.ddl); err != nil {
				return err
			}
		}
	}
	return nil
}
