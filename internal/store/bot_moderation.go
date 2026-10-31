package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"telegram-antispam-bot/internal/telegram"
)

type LinkedDelete struct {
	ActionID, RevisionID, SourceActionID int64
	MessageID                            int
}
type BotEffects struct {
	BotID            int64
	AddedToBlacklist bool
	LinkedDelete     *LinkedDelete
}
type BlacklistReport struct {
	LinkedDelete      *LinkedDelete
	Handled           bool
	BotID, Generation int64
	Status            string
	Actions           []PlannedAction
}

func planBotDeleteTx(ctx context.Context, tx *sql.Tx, j Job, kind string, bot, generation, sourceAction int64, mode string) (int64, *PlannedAction, error) {
	ids := make([]string, 0, len(j.Snapshot.Policy.Rules))
	for id := range j.Snapshot.Policy.Rules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rule := j.Snapshot.Policy.Rules[id]
		if !rule.Enabled || rule.Action != "delete" {
			continue
		}
		status := "recorded"
		if mode == "enforce" {
			status = "pending"
		}
		raw, err := json.Marshal(rule)
		if err != nil {
			return 0, nil, err
		}
		inserted, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO actions(revision_id,chat_id,message_id,user_id,rule_id,rule,kind,mode,until_date,status,created_at) VALUES(?,?,?,?,?,?,'delete',?,0,?,?)`, j.ID, j.ChatID, j.MessageID, j.UserID, id, string(raw), mode, status, time.Now().UnixMilli())
		if err != nil {
			return 0, nil, err
		}
		n, err := inserted.RowsAffected()
		if err != nil {
			return 0, nil, err
		}
		if n == 0 {
			var existing int64
			err := tx.QueryRowContext(ctx, `SELECT id FROM actions WHERE revision_id=? AND kind='delete'`, j.ID).Scan(&existing)
			return existing, nil, err
		}
		action, err := inserted.LastInsertId()
		if err != nil {
			return 0, nil, err
		}
		var source any
		if sourceAction > 0 {
			source = sourceAction
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO action_evidence(action_id,kind,bot_id,generation,source_action_id) VALUES(?,?,?,?,?)`, action, kind, bot, generation, source); err != nil {
			return 0, nil, err
		}
		return action, &PlannedAction{ID: action, RuleID: id, Kind: "delete", Status: status}, nil
	}
	return 0, nil, nil
}

func applyBlacklistTx(ctx context.Context, tx *sql.Tx, j Job, m *telegram.Message) (BlacklistReport, error) {
	var report BlacklistReport
	if !m.IsGroup() || m.BotSenderID() == 0 || m.IsAutomaticForward {
		return report, nil
	}
	entry, err := blacklistTx(ctx, tx, m.BotSenderID())
	if err != nil {
		return report, err
	}
	if entry == nil || !entry.Active {
		return report, nil
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM revisions WHERE id=?`, j.ID).Scan(&status); err != nil {
		return report, err
	}
	if status != "pending" && status != "running" {
		return report, nil
	}
	var latest int64
	if err := tx.QueryRowContext(ctx, `SELECT max(id) FROM revisions WHERE chat_id=? AND message_id=?`, j.ChatID, j.MessageID).Scan(&latest); err != nil {
		return report, err
	}
	status = "succeeded"
	if latest != j.ID {
		status = "superseded"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE revisions SET status=?,snapshot=json_set(snapshot,'$.skip_reason','bot_blacklist') WHERE id=?`, status, j.ID); err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO blacklist_matches(revision_id,bot_id,generation,created_at) VALUES(?,?,?,?)`, j.ID, entry.ID, entry.Generation, time.Now().UnixMilli()); err != nil {
		return report, err
	}
	bot := botIdentity(m)
	if _, err := tx.ExecContext(ctx, `UPDATE bot_blacklist SET username=?,name=? WHERE bot_id=?`, bot.Username, bot.Name, bot.ID); err != nil {
		return report, err
	}
	report = BlacklistReport{Handled: true, BotID: entry.ID, Generation: entry.Generation, Status: status}
	if status == "succeeded" {
		id, planned, err := planBotDeleteTx(ctx, tx, j, "bot_blacklist", entry.ID, entry.Generation, 0, j.Snapshot.Policy.Mode)
		if err != nil {
			return BlacklistReport{}, err
		}
		if planned != nil {
			report.Actions = append(report.Actions, *planned)
			if planned.Status == "recorded" {
				report.LinkedDelete, err = rememberInvocationTx(ctx, tx, id, j)
				if err != nil {
					return BlacklistReport{}, err
				}
			}
		}
	}
	return report, nil
}

func (s *Store) ApplyBotBlacklist(ctx context.Context, j Job) (BlacklistReport, error) {
	var m telegram.Message
	if err := json.Unmarshal(j.Raw, &m); err != nil {
		return BlacklistReport{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BlacklistReport{}, err
	}
	defer tx.Rollback()
	report, err := applyBlacklistTx(ctx, tx, j, &m)
	if err != nil {
		return BlacklistReport{}, err
	}
	if err := tx.Commit(); err != nil {
		return BlacklistReport{}, err
	}
	return report, nil
}

func invocationTargetEligible(j Job, m *telegram.Message) bool {
	return (j.Status == "pending" || j.Status == "running" || j.Status == "succeeded") && m.IsGroup() && m.From != nil && !m.From.IsBot && m.UserID() > 0 && m.SenderChat == nil && !m.IsAutomaticForward
}

func resolveInvocationsTx(ctx context.Context, tx *sql.Tx, j Job) (*LinkedDelete, error) {
	var target telegram.Message
	if err := json.Unmarshal(j.Raw, &target); err != nil {
		return nil, err
	}
	if !invocationTargetEligible(j, &target) {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT i.source_action_id,i.bot_id,i.user_id,i.fingerprint,a.mode,a.status
 FROM bot_invocations i JOIN actions a ON a.id=i.source_action_id
 WHERE i.chat_id=? AND i.message_id=? AND i.target_action_id IS NULL ORDER BY i.source_action_id`, j.ChatID, j.MessageID)
	if err != nil {
		return nil, err
	}
	type proof struct {
		action, bot, user         int64
		fingerprint, mode, status string
	}
	var proofs []proof
	for rows.Next() {
		var p proof
		if err := rows.Scan(&p.action, &p.bot, &p.user, &p.fingerprint, &p.mode, &p.status); err != nil {
			rows.Close()
			return nil, err
		}
		proofs = append(proofs, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var created *LinkedDelete
	for _, p := range proofs {
		if p.user != j.UserID || p.fingerprint != target.InvocationFingerprint() {
			continue
		}
		if p.status != "succeeded" && p.status != "recorded" {
			continue
		}
		mode := "record"
		if j.Snapshot.Policy.Mode == "enforce" && p.mode == "enforce" && p.status == "succeeded" {
			mode = "enforce"
		}
		id, planned, err := planBotDeleteTx(ctx, tx, j, "bot_invocation", p.bot, 0, p.action, mode)
		if err != nil {
			return nil, err
		}
		if id == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE bot_invocations SET target_action_id=? WHERE source_action_id=?`, id, p.action); err != nil {
			return nil, err
		}
		if planned != nil {
			created = &LinkedDelete{ActionID: id, RevisionID: j.ID, SourceActionID: p.action, MessageID: j.MessageID}
		}
	}
	return created, nil
}

func rememberInvocationTx(ctx context.Context, tx *sql.Tx, sourceAction int64, j Job) (*LinkedDelete, error) {
	var m telegram.Message
	if err := json.Unmarshal(j.Raw, &m); err != nil {
		return nil, err
	}
	caller := m.GuestInvocation()
	if caller == nil {
		return nil, nil
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO bot_invocations(source_action_id,bot_id,chat_id,message_id,user_id,fingerprint,created_at) VALUES(?,?,?,?,?,?,?)`, sourceAction, m.BotSenderID(), caller.Chat.ID, caller.ID, caller.UserID(), caller.InvocationFingerprint(), time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	target, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM revisions WHERE chat_id=? AND message_id=? ORDER BY id DESC LIMIT 1`, caller.Chat.ID, caller.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return resolveInvocationsTx(ctx, tx, *target)
}

// CompleteAction commits the Telegram result and its blacklist/link effects
// together. Importing historical results deliberately does not use this path.
func (s *Store) CompleteAction(ctx context.Context, id int64, status, detail string) (BotEffects, error) {
	var effects BotEffects
	if status != "succeeded" && status != "failed" && status != "skipped" {
		return effects, errors.New("invalid action outcome")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return effects, err
	}
	defer tx.Rollback()
	changed, err := tx.ExecContext(ctx, `UPDATE actions SET status=?,detail=? WHERE id=? AND status='executing'`, status, detail, id)
	if err != nil {
		return effects, err
	}
	n, err := changed.RowsAffected()
	if err != nil {
		return effects, err
	}
	if n > 0 && status == "succeeded" {
		var revision int64
		var kind, mode, evidence string
		if err := tx.QueryRowContext(ctx, `SELECT a.revision_id,a.kind,a.mode,coalesce(e.kind,'') FROM actions a LEFT JOIN action_evidence e ON e.action_id=a.id WHERE a.id=?`, id).Scan(&revision, &kind, &mode, &evidence); err != nil {
			return effects, err
		}
		if kind == "delete" && mode == "enforce" {
			j, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM revisions WHERE id=?`, revision))
			if err != nil {
				return effects, err
			}
			var m telegram.Message
			if err := json.Unmarshal(j.Raw, &m); err != nil {
				return effects, err
			}
			if m.BotSenderID() > 0 && m.IsGroup() && !m.IsAutomaticForward {
				effects.BotID = m.BotSenderID()
				if evidence == "" {
					effects.AddedToBlacklist, err = learnBotTx(ctx, tx, &m, j.ID, id, "automatic")
					if err != nil {
						return BotEffects{}, err
					}
				}
				effects.LinkedDelete, err = rememberInvocationTx(ctx, tx, id, *j)
				if err != nil {
					return BotEffects{}, err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return BotEffects{}, err
	}
	return effects, nil
}

func (s *Store) BotActionCurrent(ctx context.Context, a Action) (bool, string, error) {
	checkEntry := func(bot, generation int64) (bool, string, error) {
		entry, err := s.BotBlacklist(ctx, bot)
		if err != nil {
			return false, "", err
		}
		if entry == nil || !entry.Active || entry.Generation != generation {
			return false, "blacklist entry changed", nil
		}
		return true, "", nil
	}
	switch a.EvidenceKind {
	case "":
		return true, "", nil
	case "bot_blacklist":
		return checkEntry(a.EvidenceBotID, a.EvidenceGeneration)
	case "bot_invocation":
		var user, sourceBot, generation int64
		var fingerprint, mode, status, sourceKind string
		err := s.db.QueryRowContext(ctx, `SELECT i.user_id,i.fingerprint,source.mode,source.status,coalesce(e.kind,''),coalesce(e.bot_id,0),coalesce(e.generation,0)
 FROM bot_invocations i JOIN actions source ON source.id=i.source_action_id LEFT JOIN action_evidence e ON e.action_id=source.id
 WHERE i.source_action_id=? AND i.target_action_id=?`, a.SourceActionID, a.ID).Scan(&user, &fingerprint, &mode, &status, &sourceKind, &sourceBot, &generation)
		if errors.Is(err, sql.ErrNoRows) {
			return false, "invocation evidence missing", nil
		}
		if err != nil {
			return false, "", err
		}
		if mode != "enforce" || status != "succeeded" {
			return false, "source advertisement deletion is unconfirmed", nil
		}
		j, err := s.Revision(ctx, a.RevisionID)
		if err != nil {
			return false, "", err
		}
		var target telegram.Message
		if err := json.Unmarshal(j.Raw, &target); err != nil {
			return false, "", err
		}
		if !invocationTargetEligible(*j, &target) || user != a.UserID || fingerprint != target.InvocationFingerprint() {
			return false, "invocation target changed", nil
		}
		if sourceKind == "bot_blacklist" {
			return checkEntry(sourceBot, generation)
		}
		if sourceKind != "" {
			return false, "unsupported invocation evidence", nil
		}
		return true, "", nil
	default:
		return false, "unsupported action evidence", nil
	}
}
