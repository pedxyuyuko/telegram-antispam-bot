package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/expression"
)

func (s *Store) Finish(ctx context.Context, j Job, result decision.Result, inferenceErr error, now time.Time, retrySeconds int) (FinishReport, error) {
	var report FinishReport
	if err := s.finish(ctx, j, result, inferenceErr, now, retrySeconds, &report); err != nil {
		return FinishReport{}, err
	}
	return report, nil
}

func (s *Store) finish(ctx context.Context, j Job, result decision.Result, inferenceErr error, now time.Time, retrySeconds int, report *FinishReport) error {
	var evaluation decision.Evaluation
	if inferenceErr == nil {
		var err error
		evaluation, err = decision.EvaluatePolicy(j.Snapshot.Questions, j.Snapshot.Policy, result)
		if err != nil {
			return fmt.Errorf("evaluate archived policy: %w", err)
		}
		report.Evaluation = &evaluation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	probs, _ := json.Marshal(result.Probabilities)
	reason := ""
	if inferenceErr != nil {
		reason = inferenceErr.Error()
	}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO attempts(revision_id,created_at,request,response,probabilities,error,duration_ms) VALUES(?,?,?,?,?,?,?)`, j.ID, now.UnixMilli(), string(result.Request), string(result.Response), string(probs), reason, result.Duration.Milliseconds())
	if err != nil {
		return err
	}
	report.AttemptID, err = inserted.LastInsertId()
	if err != nil {
		return err
	}
	if inferenceErr == nil {
		raw, err := json.Marshal(evaluation)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO evaluations(attempt_id,result) VALUES(?,?)`, report.AttemptID, string(raw)); err != nil {
			return err
		}
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM revisions WHERE id=?`, j.ID).Scan(&status); err != nil {
		return err
	}
	report.Status = status
	if inferenceErr != nil {
		next := now.Add(time.Duration(retrySeconds) * time.Second).UnixMilli()
		report.RetryAt = next
		report.Reason = "model_failed"
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('model_retry_at',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, next); err != nil {
			return err
		}
		if status == "running" {
			if _, err := tx.ExecContext(ctx, `UPDATE revisions SET status='pending',available_at=? WHERE id=?`, next, j.ID); err != nil {
				return err
			}
			report.Status = "pending"
		}
		return tx.Commit()
	}
	if status != "running" {
		report.Reason = "revision_no_longer_running"
		return tx.Commit()
	}
	var latest int64
	if err := tx.QueryRowContext(ctx, `SELECT max(id) FROM revisions WHERE chat_id=? AND message_id=?`, j.ChatID, j.MessageID).Scan(&latest); err != nil {
		return err
	}
	if latest != j.ID {
		if _, err := tx.ExecContext(ctx, `UPDATE revisions SET status='superseded' WHERE id=?`, j.ID); err != nil {
			return err
		}
		report.Status = "superseded"
		report.Reason = "newer_revision_exists"
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE revisions SET status='succeeded' WHERE id=?`, j.ID); err != nil {
		return err
	}
	policy := j.Snapshot.Policy
	report.Status = "succeeded"
	report.Reason = "condition_" + string(evaluation.Condition)
	if evaluation.Condition != expression.True {
		return tx.Commit()
	}
	ids := make([]string, 0, len(policy.Rules))
	for id := range policy.Rules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	type candidate struct {
		id   string
		rule config.Rule
	}
	chosen := map[string]candidate{}
	for _, id := range ids {
		r := policy.Rules[id]
		p, present := evaluation.Values["p."+r.Question]
		trace := RuleReport{ID: id, Question: r.Question, Threshold: r.Threshold, Required: r.Count, WindowSeconds: r.WindowSeconds}
		if present {
			trace.Probability = &p
		}
		if !r.Enabled {
			trace.Status = "disabled"
			report.Rules = append(report.Rules, trace)
			continue
		}
		if r.Question != "" && (!present || p < r.Threshold) {
			trace.Status = "below_probability"
			if !present {
				trace.Status = "unknown_answer"
			}
			report.Rules = append(report.Rules, trace)
			continue
		}
		// Changing the question or threshold starts a separate evidence series;
		// changing a punishment leaves the same underlying hit history usable.
		key := decision.PolicyKey(policy, j.Snapshot.Questions, r)
		hit, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO hits(chat_id,message_id,sender,rule_id,rule_key,mode,occurred_at) VALUES(?,?,?,?,?,?,?)`, j.ChatID, j.MessageID, j.Sender, id, key, policy.Mode, j.ReceivedAt)
		if err != nil {
			return err
		}
		added, err := hit.RowsAffected()
		if err != nil {
			return err
		}
		trace.NewHit = added > 0
		cutoff := int64(0)
		if r.WindowSeconds > 0 {
			cutoff = now.Add(-time.Duration(r.WindowSeconds) * time.Second).UnixMilli()
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM hits WHERE chat_id=? AND sender=? AND rule_id=? AND rule_key=? AND mode=? AND occurred_at>=? AND occurred_at<=?`, j.ChatID, j.Sender, id, key, policy.Mode, cutoff, now.UnixMilli()).Scan(&count); err != nil {
			return err
		}
		if count < r.Count {
			trace.Status = "below_count"
			trace.Hits = count
			report.Rules = append(report.Rules, trace)
			continue
		}
		trace.Status = "eligible"
		trace.Hits = count
		report.Rules = append(report.Rules, trace)
		old, ok := chosen[r.Action]
		if !ok || r.DurationSeconds > old.rule.DurationSeconds {
			chosen[r.Action] = candidate{id, r}
		}
	}
	if _, kick := chosen["kick"]; kick {
		delete(chosen, "mute")
	}
	for _, kind := range []string{"delete", "mute", "kick"} {
		c, ok := chosen[kind]
		if !ok {
			continue
		}
		actionStatus := "pending"
		if policy.Mode != "enforce" {
			actionStatus = "recorded"
		}
		rule, _ := json.Marshal(c.rule)
		until := int64(0)
		if kind == "mute" {
			until = now.Unix() + int64(c.rule.DurationSeconds)
		}
		inserted, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO actions(revision_id,chat_id,message_id,user_id,rule_id,rule,kind,mode,until_date,status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, j.ID, j.ChatID, j.MessageID, j.UserID, c.id, string(rule), kind, policy.Mode, until, actionStatus, now.UnixMilli())
		if err != nil {
			return err
		}
		added, err := inserted.RowsAffected()
		if err != nil {
			return err
		}
		if added > 0 {
			id, err := inserted.LastInsertId()
			if err != nil {
				return err
			}
			report.Actions = append(report.Actions, PlannedAction{ID: id, RuleID: c.id, Kind: kind, Status: actionStatus, Until: until})
		}
	}
	return tx.Commit()
}

func (s *Store) TakeAction(ctx context.Context) (*Action, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var a Action
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT a.id,a.revision_id,a.chat_id,a.message_id,a.user_id,a.rule_id,a.rule,a.kind,a.until_date FROM actions a
 WHERE a.status='pending' AND NOT EXISTS (SELECT 1 FROM actions b WHERE b.chat_id=a.chat_id AND b.message_id=a.message_id AND b.kind=a.kind AND b.status='executing')
 ORDER BY a.id LIMIT 1`).Scan(&a.ID, &a.RevisionID, &a.ChatID, &a.MessageID, &a.UserID, &a.RuleID, &raw, &a.Kind, &a.Until)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &a.Rule); err != nil {
		return nil, err
	}
	var sent int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM actions WHERE chat_id=? AND message_id=? AND kind=? AND mode='enforce' AND status IN ('succeeded','failed','uncertain')`, a.ChatID, a.MessageID, a.Kind).Scan(&sent); err != nil {
		return nil, err
	}
	if sent > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET status='skipped',detail='an earlier version already dispatched this action' WHERE id=?`, a.ID); err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE actions SET status='executing' WHERE id=?`, a.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &a, nil
}
func (s *Store) ActionCurrent(ctx context.Context, a Action) (bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT max(id) FROM revisions WHERE chat_id=? AND message_id=?`, a.ChatID, a.MessageID).Scan(&id)
	return id == a.RevisionID, err
}
func (s *Store) FinishAction(ctx context.Context, id int64, status, detail string) error {
	if status != "succeeded" && status != "failed" && status != "skipped" {
		return errors.New("invalid action outcome")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE actions SET status=?,detail=? WHERE id=? AND status='executing'`, status, detail, id)
	return err
}
