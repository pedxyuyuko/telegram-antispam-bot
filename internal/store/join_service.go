package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/expression"
	"telegram-antispam-bot/internal/telegram"
)

const joinServiceSchema = `
CREATE INDEX IF NOT EXISTS join_service_lookup ON revisions(chat_id,json_extract(raw,'$.date')) WHERE json_type(snapshot,'$.join')='object';
CREATE TABLE IF NOT EXISTS join_service_actions(
 id INTEGER PRIMARY KEY,revision_id INTEGER NOT NULL UNIQUE REFERENCES revisions(id),
 chat_id INTEGER NOT NULL,message_id INTEGER NOT NULL,rule_id TEXT NOT NULL,rule TEXT NOT NULL,
 evidence TEXT NOT NULL,mode TEXT NOT NULL,status TEXT NOT NULL,detail TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,
 UNIQUE(chat_id,message_id));
CREATE INDEX IF NOT EXISTS join_service_pending ON join_service_actions(status,id);
PRAGMA user_version=9;`

type JoinServiceEvidence struct {
	JoinID    int64 `json:"join_id"`
	AttemptID int64 `json:"attempt_id"`
}

type JoinServiceAction struct {
	ID, RevisionID, ChatID int64
	MessageID              int
	RuleID                 string
	Rule                   config.Rule
	Evidence               []JoinServiceEvidence
}

func joinDeleteRule(p config.JoinPolicy) (string, config.Rule, bool) {
	var ids []string
	for id, r := range p.Rules {
		if r.Enabled && r.Action == "delete" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return "", config.Rule{}, false
	}
	return ids[0], p.Rules[ids[0]], true
}

func cleanupCutoffs(ctx context.Context, tx *sql.Tx, chat int64) (join, revision int64, err error) {
	key := strconv.FormatInt(chat, 10)
	err = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT CAST(value AS INTEGER) FROM meta WHERE key=?),0),coalesce((SELECT CAST(value AS INTEGER) FROM meta WHERE key=?),0)`, "join_cleanup_join_cutoff:"+key, "join_cleanup_revision_cutoff:"+key).Scan(&join, &revision)
	return
}

// Cancellation also closes authority for hints/decisions that arrive later,
// preserving the off/on boundary without rewriting any archived policy.
func cancelJoinServiceTx(ctx context.Context, tx *sql.Tx, chat int64, reason string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE join_service_actions SET status='skipped',detail=? WHERE chat_id=? AND status IN ('pending','checking')`, reason, chat); err != nil {
		return err
	}
	key := strconv.FormatInt(chat, 10)
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,(SELECT coalesce(max(id),0) FROM join_events WHERE chat_id=?)) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "join_cleanup_join_cutoff:"+key, chat); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,(SELECT coalesce(max(id),0) FROM revisions WHERE chat_id=?)) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "join_cleanup_revision_cutoff:"+key, chat)
	return err
}

func ambiguousPendingArrival(ctx context.Context, tx *sql.Tx, chat, user, date, knownUpdate int64) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT raw FROM updates WHERE processed=0 AND update_id<>? AND json_extract(raw,'$.chat_member.chat.id')=? AND json_extract(raw,'$.chat_member.new_chat_member.user.id')=? AND json_extract(raw,'$.chat_member.date')=?`, knownUpdate, chat, user, date)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		u, err := telegram.Parse(raw)
		if err != nil {
			return false, err
		}
		if u.ChatMember != nil && u.ChatMember.Valid() && u.ChatMember.Joined() {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Every person named in the service message must have one unambiguous arrival
// at its exact Telegram timestamp and a successful violating decision. Shared
// hints containing normal or unknown members are preserved.
func joinServiceEvidenceTx(ctx context.Context, tx *sql.Tx, j Job, ruleID string, rule config.Rule) ([]JoinServiceEvidence, string, bool, error) {
	snap := j.Snapshot.Join
	if snap == nil || !snap.Policy.Enabled || j.Status != "ignored" || j.Snapshot.SkipReason != "membership_service" {
		return nil, "", false, nil
	}
	if saved, ok := snap.Policy.Rules[ruleID]; !ok || saved != rule || !saved.Enabled || saved.Action != "delete" {
		return nil, "", false, nil
	}
	var m telegram.Message
	if err := json.Unmarshal(j.Raw, &m); err != nil {
		return nil, "", false, err
	}
	if !m.IsGroup() || m.ID <= 0 || m.Date <= 0 || m.EditDate != 0 || len(m.NewChatMembers) == 0 {
		return nil, "", false, nil
	}
	var latest int64
	if err := tx.QueryRowContext(ctx, `SELECT max(id) FROM revisions WHERE chat_id=? AND message_id=?`, j.ChatID, j.MessageID).Scan(&latest); err != nil {
		return nil, "", false, err
	}
	if latest != j.ID {
		return nil, "", false, nil
	}
	ids := make([]int64, 0, len(m.NewChatMembers))
	seen := map[int64]bool{}
	for _, u := range m.NewChatMembers {
		if u.ID <= 0 {
			return nil, "", false, nil
		}
		if !seen[u.ID] {
			seen[u.ID] = true
			ids = append(ids, u.ID)
		}
	}
	sort.Slice(ids, func(i, k int) bool { return ids[i] < ids[k] })
	mode := snap.Policy.Mode
	var evidence []JoinServiceEvidence
	joinCutoff, revisionCutoff, err := cleanupCutoffs(ctx, tx, j.ChatID)
	if err != nil {
		return nil, "", false, err
	}
	for _, user := range ids {
		var count int
		var joinID int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(min(id),0) FROM join_events WHERE chat_id=? AND user_id=? AND event_at=? AND is_join=1`, j.ChatID, user, m.Date).Scan(&count, &joinID); err != nil {
			return nil, "", false, err
		}
		if count != 1 {
			return nil, "", false, nil
		}
		joined, err := scanJoin(tx.QueryRowContext(ctx, `SELECT `+joinColumns+` FROM join_events WHERE id=?`, joinID))
		if err != nil {
			return nil, "", false, err
		}
		if joined.Status != "succeeded" || !joined.Snapshot.Policy.Enabled {
			return nil, "", false, nil
		}
		saved, ok := joined.Snapshot.Policy.Rules[ruleID]
		if !ok || saved != rule || decision.PolicyKey(joined.Snapshot.Policy.Policy, joined.Snapshot.Questions, saved) != decision.PolicyKey(snap.Policy.Policy, snap.Questions, rule) {
			return nil, "", false, nil
		}
		if ambiguous, err := ambiguousPendingArrival(ctx, tx, j.ChatID, user, m.Date, joined.UpdateID); err != nil || ambiguous {
			return nil, "", false, err
		}
		var attemptID int64
		var raw []byte
		err = tx.QueryRowContext(ctx, `SELECT id,evaluation FROM join_attempts WHERE join_id=? AND error='' ORDER BY id LIMIT 1`, joinID).Scan(&attemptID, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", false, nil
		}
		if err != nil {
			return nil, "", false, err
		}
		var e decision.Evaluation
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, "", false, err
		}
		if e.Condition != expression.True {
			return nil, "", false, nil
		}
		if joined.Snapshot.Policy.Mode != "enforce" {
			mode = "record"
		}
		evidence = append(evidence, JoinServiceEvidence{JoinID: joinID, AttemptID: attemptID})
	}
	if mode == "enforce" {
		if j.ID <= revisionCutoff {
			return nil, "", false, nil
		}
		for _, e := range evidence {
			if e.JoinID <= joinCutoff {
				return nil, "", false, nil
			}
		}
	}
	return evidence, mode, true, nil
}

func planJoinServiceTx(ctx context.Context, tx *sql.Tx, j Job) error {
	if j.Snapshot.Join == nil {
		return nil
	}
	id, rule, ok := joinDeleteRule(j.Snapshot.Join.Policy)
	if !ok {
		return nil
	}
	evidence, mode, eligible, err := joinServiceEvidenceTx(ctx, tx, j, id, rule)
	if err != nil || !eligible {
		return err
	}
	status := "recorded"
	if mode == "enforce" {
		status = "pending"
	}
	rawRule, _ := json.Marshal(rule)
	rawEvidence, _ := json.Marshal(evidence)
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO join_service_actions(revision_id,chat_id,message_id,rule_id,rule,evidence,mode,status,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, j.ID, j.ChatID, j.MessageID, id, string(rawRule), string(rawEvidence), mode, status, time.Now().UnixMilli())
	return err
}

func planJoinServicesForArrivalTx(ctx context.Context, tx *sql.Tx, j JoinJob) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM revisions WHERE chat_id=? AND json_extract(raw,'$.date')=? AND json_type(snapshot,'$.join')='object'`, j.ChatID, j.Event.Date)
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
		source, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM revisions WHERE id=?`, id))
		if err != nil {
			return err
		}
		if err := planJoinServiceTx(ctx, tx, *source); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) TakeJoinServiceAction(ctx context.Context) (*JoinServiceAction, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var a JoinServiceAction
	var rule, evidence []byte
	err = tx.QueryRowContext(ctx, `SELECT id,revision_id,chat_id,message_id,rule_id,rule,evidence FROM join_service_actions WHERE status='pending' ORDER BY id LIMIT 1`).Scan(&a.ID, &a.RevisionID, &a.ChatID, &a.MessageID, &a.RuleID, &rule, &evidence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rule, &a.Rule); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(evidence, &a.Evidence); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE join_service_actions SET status='checking' WHERE id=?`, a.ID); err != nil {
		return nil, err
	}
	return &a, tx.Commit()
}

func joinServiceActionCurrentTx(ctx context.Context, tx *sql.Tx, a JoinServiceAction, cfg config.Config) (bool, error) {
	j, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM revisions WHERE id=?`, a.RevisionID))
	if err != nil {
		return false, err
	}
	p := cfg.ForJoinGroup(a.ChatID)
	rule, ok := p.Rules[a.RuleID]
	if j.Snapshot.Join == nil || j.ChatID != a.ChatID || j.MessageID != a.MessageID || !p.Enabled || p.Mode != "enforce" || !ok || !rule.Enabled || rule != a.Rule || rule.Action != "delete" {
		return false, nil
	}
	questions, err := cfg.SelectedQuestions(p.Policy)
	if err != nil {
		return false, err
	}
	if decision.PolicyKey(p.Policy, questions, rule) != decision.PolicyKey(j.Snapshot.Join.Policy.Policy, j.Snapshot.Join.Questions, rule) {
		return false, nil
	}
	var m telegram.Message
	if err := json.Unmarshal(j.Raw, &m); err != nil {
		return false, err
	}
	for _, u := range m.NewChatMembers {
		if cfg.IsOwner(u.ID) {
			return false, nil
		}
	}
	evidence, mode, eligible, err := joinServiceEvidenceTx(ctx, tx, *j, a.RuleID, a.Rule)
	return eligible && mode == "enforce" && reflect.DeepEqual(evidence, a.Evidence), err
}

func (s *Store) JoinServiceActionCurrent(ctx context.Context, a JoinServiceAction, cfg config.Config) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	return joinServiceActionCurrentTx(ctx, tx, a, cfg)
}

func (s *Store) StartJoinServiceAction(ctx context.Context, a JoinServiceAction, cfg config.Config) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	valid, err := joinServiceActionCurrentTx(ctx, tx, a, cfg)
	if err != nil || !valid {
		return false, err
	}
	r, err := tx.ExecContext(ctx, `UPDATE join_service_actions SET status='executing' WHERE id=? AND revision_id=? AND status='checking'`, a.ID, a.RevisionID)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, tx.Commit()
}

func (s *Store) CompleteJoinServiceAction(ctx context.Context, id int64, status, detail string) error {
	if status != "skipped" && status != "succeeded" && status != "failed" {
		return errors.New("invalid join service deletion outcome")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE join_service_actions SET status=?,detail=? WHERE id=? AND status IN ('checking','executing')`, status, detail, id)
	return err
}
