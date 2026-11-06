package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/telegram"
)

const contentCacheSchema = `
CREATE TABLE IF NOT EXISTS content_cache(
 id INTEGER PRIMARY KEY,content_hash TEXT NOT NULL,material TEXT NOT NULL,
 model_id TEXT NOT NULL,model_endpoint TEXT NOT NULL,policy_key TEXT NOT NULL,
 answers TEXT NOT NULL,source_attempt_id INTEGER NOT NULL REFERENCES attempts(id),created_at INTEGER NOT NULL,
 UNIQUE(content_hash,model_id,model_endpoint,policy_key));
CREATE TABLE IF NOT EXISTS content_cache_matches(
 revision_id INTEGER PRIMARY KEY REFERENCES revisions(id),cache_id INTEGER NOT NULL REFERENCES content_cache(id),
 source_attempt_id INTEGER NOT NULL REFERENCES attempts(id),evaluation TEXT NOT NULL,created_at INTEGER NOT NULL);
PRAGMA user_version=6;`

type ContentCacheHit struct {
	CacheID, SourceAttemptID int64
	ModelID                  string
}
type contentEntry struct {
	ID, SourceAttemptID int64
	ModelID             string
	Answers             []byte
}

func jobFingerprint(j Job) (telegram.ContentFingerprint, error) {
	var m telegram.Message
	if err := json.Unmarshal(j.Raw, &m); err != nil {
		return telegram.ContentFingerprint{}, err
	}
	if !m.IsGroup() || m.IsAutomaticForward {
		return telegram.ContentFingerprint{}, nil
	}
	if j.Snapshot.ContextVersion == telegram.SegmentedContextVersion {
		return m.SegmentedContentFingerprint(j.State)
	}
	if j.Snapshot.ContextVersion > telegram.SegmentedContextVersion {
		return telegram.ContentFingerprint{}, nil
	}
	return m.ContentFingerprint()
}

func learnContentTx(ctx context.Context, tx *sql.Tx, j Job, result decision.Result, attempt int64, now time.Time) error {
	if result.ModelID == "" || result.ModelEndpoint == "" || attempt <= 0 {
		return nil
	}
	projected, ok, err := decision.ContentProjection(j.Snapshot.Questions, j.Snapshot.Policy, result)
	if err != nil || !ok {
		return err
	}
	fingerprint, err := jobFingerprint(j)
	if err != nil || fingerprint.Hash == "" {
		return nil
	}
	answers, err := json.Marshal(projected.Answers)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO content_cache(content_hash,material,model_id,model_endpoint,policy_key,answers,source_attempt_id,created_at) VALUES(?,?,?,?,?,?,?,?)`, fingerprint.Hash, string(fingerprint.Material), result.ModelID, strings.TrimRight(result.ModelEndpoint, "/"), decision.ContentPolicyKey(j.Snapshot.Questions, j.Snapshot.Policy), string(answers), attempt, now.UnixMilli())
	return err
}

func applyContentCacheTx(ctx context.Context, tx *sql.Tx, j Job, model config.Model) (FinishReport, bool, error) {
	var report FinishReport
	if model.Name == "" || model.BaseURL == "" {
		return report, false, nil
	}
	fingerprint, err := jobFingerprint(j)
	if err != nil || fingerprint.Hash == "" {
		return report, false, nil
	}
	var entry contentEntry
	err = tx.QueryRowContext(ctx, `SELECT id,source_attempt_id,model_id,answers FROM content_cache WHERE content_hash=? AND material=? AND model_id=? AND model_endpoint=? AND policy_key=?`, fingerprint.Hash, string(fingerprint.Material), model.Name, strings.TrimRight(model.BaseURL, "/"), decision.ContentPolicyKey(j.Snapshot.Questions, j.Snapshot.Policy)).Scan(&entry.ID, &entry.SourceAttemptID, &entry.ModelID, &entry.Answers)
	if errors.Is(err, sql.ErrNoRows) {
		return report, false, nil
	}
	if err != nil {
		return report, false, err
	}
	var answers map[string]decision.Answer
	if err := json.Unmarshal(entry.Answers, &answers); err != nil {
		return report, false, err
	}
	result, valid, err := decision.ContentProjection(j.Snapshot.Questions, j.Snapshot.Policy, decision.Result{Answers: answers})
	if err != nil || !valid {
		return report, false, err
	}
	evaluation, err := decision.EvaluatePolicy(j.Snapshot.Questions, j.Snapshot.Policy, result)
	if err != nil {
		return report, false, err
	}
	report.Evaluation = &evaluation
	if err := finishSuccessTx(ctx, tx, j, result, evaluation, time.Now(), &report, &entry); err != nil {
		return FinishReport{}, false, err
	}
	return report, true, nil
}

func (s *Store) ApplyContentCache(ctx context.Context, j Job, model config.Model) (FinishReport, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FinishReport{}, false, err
	}
	defer tx.Rollback()
	report, hit, err := applyContentCacheTx(ctx, tx, j, model)
	if err != nil {
		return FinishReport{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return FinishReport{}, false, err
	}
	return report, hit, nil
}

// Cached source authority is checked for direct actions and every candidate
// invocation source, using the current group's classifier and model namespace.
func (s *Store) contentActionCurrent(ctx context.Context, revision, chat int64, cfg *config.Config) (bool, string, error) {
	var model, endpoint, policyKey string
	err := s.db.QueryRowContext(ctx, `SELECT c.model_id,c.model_endpoint,c.policy_key FROM content_cache_matches m JOIN content_cache c ON c.id=m.cache_id WHERE m.revision_id=?`, revision).Scan(&model, &endpoint, &policyKey)
	if errors.Is(err, sql.ErrNoRows) {
		return true, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if cfg == nil {
		return false, "cache authority requires current configuration", nil
	}
	p := cfg.ForGroup(chat)
	questions, err := cfg.SelectedQuestions(p)
	if err != nil {
		return false, "", err
	}
	valid := model == cfg.Model.Name && endpoint == strings.TrimRight(cfg.Model.BaseURL, "/") && policyKey == decision.ContentPolicyKey(questions, p)
	return valid, "cached classifier or model changed", nil
}
