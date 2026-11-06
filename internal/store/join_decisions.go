package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/expression"
)

// Includes episode-ending raw updates whose inbox processing has not yet caught
// up. Ordinary in-group permission changes preserve the same arrival episode.
func joinCurrentTx(ctx context.Context, tx *sql.Tx, id int64) (bool, error) {
	var current bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM join_events e WHERE e.id=? AND e.is_join=1 AND NOT EXISTS(
 SELECT 1 FROM join_events n WHERE n.chat_id=e.chat_id AND n.user_id=e.user_id
 AND (n.event_at>e.event_at OR (n.event_at=e.event_at AND n.update_id>e.update_id))
 AND (n.is_join=1 OR n.present=0 OR n.role IN ('administrator','creator')))
 AND NOT EXISTS(SELECT 1 FROM updates u WHERE u.processed=0
 AND json_extract(u.raw,'$.chat_member.chat.id')=e.chat_id
	 AND json_extract(u.raw,'$.chat_member.new_chat_member.user.id')=e.user_id
	 AND (json_extract(u.raw,'$.chat_member.date')>e.event_at OR (json_extract(u.raw,'$.chat_member.date')=e.event_at AND u.update_id>e.update_id))
	 AND (
	  json_extract(u.raw,'$.chat_member.new_chat_member.status') IN ('left','kicked','administrator','creator')
	  OR (json_extract(u.raw,'$.chat_member.new_chat_member.status')='restricted' AND coalesce(json_extract(u.raw,'$.chat_member.new_chat_member.is_member'),0)=0)
	  OR ((json_extract(u.raw,'$.chat_member.old_chat_member.status') IN ('left','kicked')
	       OR (json_extract(u.raw,'$.chat_member.old_chat_member.status')='restricted' AND coalesce(json_extract(u.raw,'$.chat_member.old_chat_member.is_member'),0)=0))
	      AND (json_extract(u.raw,'$.chat_member.new_chat_member.status') IN ('member','administrator','creator')
	           OR (json_extract(u.raw,'$.chat_member.new_chat_member.status')='restricted' AND json_extract(u.raw,'$.chat_member.new_chat_member.is_member')=1)))
	 )))`, id).Scan(&current)
	return current, err
}

func (s *Store) JoinCurrent(ctx context.Context, id int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	return joinCurrentTx(ctx, tx, id)
}

func (s *Store) FinishJoin(ctx context.Context, j JoinJob, result decision.Result, inferenceErr error, now time.Time, retrySeconds int) (FinishReport, error) {
	var report FinishReport
	var evaluation decision.Evaluation
	var rawEvaluation []byte
	if inferenceErr == nil {
		var err error
		evaluation, err = decision.EvaluatePolicy(j.Snapshot.Questions, j.Snapshot.Policy.Policy, result)
		if err != nil {
			return report, err
		}
		report.Evaluation = &evaluation
		rawEvaluation, err = json.Marshal(evaluation)
		if err != nil {
			return report, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	reason := ""
	if inferenceErr != nil {
		reason = inferenceErr.Error()
	}
	answers, _ := json.Marshal(result.Answers)
	r, err := tx.ExecContext(ctx, `INSERT INTO join_attempts(join_id,created_at,request,response,answers,evaluation,error,duration_ms) VALUES(?,?,?,?,?,?,?,?)`, j.ID, now.UnixMilli(), string(result.Request), string(result.Response), string(answers), string(rawEvaluation), reason, result.Duration.Milliseconds())
	if err != nil {
		return report, err
	}
	report.AttemptID, err = r.LastInsertId()
	if err != nil {
		return report, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT status FROM join_events WHERE id=?`, j.ID).Scan(&report.Status); err != nil {
		return report, err
	}
	if report.Status != "running" {
		report.Reason = "join_no_longer_running"
		return report, tx.Commit()
	}
	if inferenceErr != nil {
		report.Status = "pending"
		report.Reason = "model_failed"
		report.RetryAt = now.Add(time.Duration(retrySeconds) * time.Second).UnixMilli()
		if _, err := tx.ExecContext(ctx, `UPDATE join_events SET status='pending',available_at=? WHERE id=?`, report.RetryAt, j.ID); err != nil {
			return report, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('join_model_retry_at',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, report.RetryAt); err != nil {
			return report, err
		}
		return report, tx.Commit()
	}
	current, err := joinCurrentTx(ctx, tx, j.ID)
	if err != nil {
		return report, err
	}
	report.Status = "succeeded"
	report.Reason = "condition_" + string(evaluation.Condition)
	if !current {
		report.Status = "superseded"
		report.Reason = "membership_episode_changed"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE join_events SET status=?,detail=? WHERE id=?`, report.Status, report.Reason, j.ID); err != nil {
		return report, err
	}
	if !current || evaluation.Condition != expression.True || !j.Snapshot.Policy.Enabled {
		return report, tx.Commit()
	}
	ids := make([]string, 0, len(j.Snapshot.Policy.Rules))
	for id := range j.Snapshot.Policy.Rules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rule := j.Snapshot.Policy.Rules[id]
		if !rule.Enabled || rule.Action != "kick" {
			continue
		}
		status := "recorded"
		if j.Snapshot.Policy.Mode == "enforce" {
			status = "pending"
		}
		raw, _ := json.Marshal(rule)
		r, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO join_actions(join_id,chat_id,user_id,rule_id,rule,mode,status,created_at) VALUES(?,?,?,?,?,?,?,?)`, j.ID, j.ChatID, j.UserID, id, string(raw), j.Snapshot.Policy.Mode, status, now.UnixMilli())
		if err != nil {
			return report, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return report, err
		}
		if n > 0 {
			actionID, err := r.LastInsertId()
			if err != nil {
				return report, err
			}
			report.Actions = append(report.Actions, PlannedAction{ID: actionID, RuleID: id, Kind: "kick", Status: status})
		}
		break
	}
	return report, tx.Commit()
}

type JoinAction struct {
	ID, JoinID, ChatID, UserID int64
	RuleID                     string
	Rule                       config.Rule
}

func (s *Store) TakeJoinAction(ctx context.Context) (*JoinAction, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var a JoinAction
	var rule []byte
	err = tx.QueryRowContext(ctx, `SELECT id,join_id,chat_id,user_id,rule_id,rule FROM join_actions WHERE status='pending' ORDER BY id LIMIT 1`).Scan(&a.ID, &a.JoinID, &a.ChatID, &a.UserID, &a.RuleID, &rule)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rule, &a.Rule); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE join_actions SET status='checking' WHERE id=?`, a.ID); err != nil {
		return nil, err
	}
	return &a, tx.Commit()
}

func (s *Store) StartJoinAction(ctx context.Context, a JoinAction) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	current, err := joinCurrentTx(ctx, tx, a.JoinID)
	if err != nil {
		return false, err
	}
	if !current {
		return false, nil
	}
	r, err := tx.ExecContext(ctx, `UPDATE join_actions SET status='executing' WHERE id=? AND join_id=? AND status='checking' AND EXISTS(SELECT 1 FROM join_events WHERE id=? AND status='succeeded')`, a.ID, a.JoinID, a.JoinID)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, tx.Commit()
}

func (s *Store) CompleteJoinAction(ctx context.Context, id int64, status, detail string) error {
	if status != "skipped" && status != "succeeded" && status != "failed" {
		return errors.New("invalid join action outcome")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE join_actions SET status=?,detail=? WHERE id=? AND status IN ('checking','executing')`, status, detail, id)
	return err
}
