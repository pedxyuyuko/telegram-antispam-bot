package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/telegram"
)

type LinkedAction struct {
	ActionID, RevisionID, SourceActionID int64
	MessageID                            int
	Kind                                 string
}
type BotEffects struct {
	BotID            int64
	AddedToBlacklist bool
	LinkedActions    []LinkedAction
}
type BlacklistReport struct {
	LinkedActions     []LinkedAction
	Handled           bool
	BotID, Generation int64
	Status            string
	Actions           []PlannedAction
}

func auditBotActionTx(ctx context.Context, tx *sql.Tx, action int64, event string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO bot_action_events(action_id,event,source_action_id,mode,status,rule_id,rule,evidence_kind,detail,created_at)
 SELECT a.id,?,e.source_action_id,a.mode,a.status,a.rule_id,a.rule,coalesce(e.kind,''),a.detail,?
 FROM actions a LEFT JOIN action_evidence e ON e.action_id=a.id WHERE a.id=?`, event, time.Now().UnixMilli(), action)
	return err
}

func planBotActionTx(ctx context.Context, tx *sql.Tx, j Job, kind string, bot, generation, sourceAction int64, mode, actionKind string, sourcePolicy *config.Policy) (int64, *PlannedAction, error) {
	ids := make([]string, 0, len(j.Snapshot.Policy.Rules))
	for id := range j.Snapshot.Policy.Rules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rule := j.Snapshot.Policy.Rules[id]
		if !rule.Enabled || rule.Action != actionKind {
			continue
		}
		if actionKind == "ban" && (rule.Target != "caller" || sourcePolicy == nil || sourcePolicy.Rules[id] != rule) {
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
		until := int64(0)
		if actionKind == "ban" && rule.DurationSeconds > 0 {
			until = time.Now().Unix() + int64(rule.DurationSeconds)
		}
		inserted, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO actions(revision_id,chat_id,message_id,user_id,rule_id,rule,kind,mode,until_date,status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, j.ID, j.ChatID, j.MessageID, j.UserID, id, string(raw), actionKind, mode, until, status, time.Now().UnixMilli())
		if err != nil {
			return 0, nil, err
		}
		n, err := inserted.RowsAffected()
		if err != nil {
			return 0, nil, err
		}
		if n == 0 {
			var existing int64
			var oldStatus, oldKind string
			err := tx.QueryRowContext(ctx, `SELECT a.id,a.status,coalesce(e.kind,'') FROM actions a LEFT JOIN action_evidence e ON e.action_id=a.id WHERE a.revision_id=? AND a.kind=?`, j.ID, actionKind).Scan(&existing, &oldStatus, &oldKind)
			if err != nil {
				return 0, nil, err
			}
			upgrade := kind == "bot_invocation" && mode == "enforce" && j.Snapshot.Policy.Mode == "enforce" && sourceAction > 0 &&
				((oldKind == "bot_invocation" && (oldStatus == "recorded" || oldStatus == "skipped")) || (oldKind == "" && oldStatus == "skipped"))
			if !upgrade {
				return existing, nil, nil
			}
			if err := auditBotActionTx(ctx, tx, existing, "before_upgrade"); err != nil {
				return 0, nil, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE actions SET mode='enforce',status='pending',rule_id=?,rule=?,until_date=?,detail='' WHERE id=?`, id, string(raw), until, existing); err != nil {
				return 0, nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO action_evidence(action_id,kind,bot_id,generation,source_action_id) VALUES(?,'bot_invocation',?,0,?) ON CONFLICT(action_id) DO UPDATE SET kind='bot_invocation',bot_id=excluded.bot_id,generation=0,source_action_id=excluded.source_action_id`, existing, bot, sourceAction); err != nil {
				return 0, nil, err
			}
			if err := auditBotActionTx(ctx, tx, existing, "upgraded"); err != nil {
				return 0, nil, err
			}
			return existing, &PlannedAction{ID: existing, RuleID: id, Kind: actionKind, Status: "pending", Until: until}, nil
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
		if err := auditBotActionTx(ctx, tx, action, "planned"); err != nil {
			return 0, nil, err
		}
		return action, &PlannedAction{ID: action, RuleID: id, Kind: actionKind, Status: status, Until: until}, nil
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
		id, planned, err := planBotActionTx(ctx, tx, j, "bot_blacklist", entry.ID, entry.Generation, 0, j.Snapshot.Policy.Mode, "delete", nil)
		if err != nil {
			return BlacklistReport{}, err
		}
		if planned != nil {
			report.Actions = append(report.Actions, *planned)
			if planned.Status == "recorded" {
				report.LinkedActions, err = rememberInvocationTx(ctx, tx, id, j)
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
	return (j.Status == "pending" || j.Status == "running" || j.Status == "succeeded" || j.Status == "evicted") && m.IsGroup() && m.From != nil && !m.From.IsBot && m.UserID() > 0 && m.SenderChat == nil && !m.IsAutomaticForward
}

func resolveInvocationsTx(ctx context.Context, tx *sql.Tx, j Job) ([]LinkedAction, error) {
	var target telegram.Message
	if err := json.Unmarshal(j.Raw, &target); err != nil {
		return nil, err
	}
	if !invocationTargetEligible(j, &target) {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT i.source_action_id,i.bot_id,i.user_id,i.fingerprint,a.mode,a.status,r.snapshot
 FROM bot_invocations i JOIN actions a ON a.id=i.source_action_id JOIN revisions r ON r.id=a.revision_id
 WHERE i.chat_id=? AND i.message_id=? ORDER BY i.source_action_id`, j.ChatID, j.MessageID)
	if err != nil {
		return nil, err
	}
	type proof struct {
		action, bot, user         int64
		fingerprint, mode, status string
		snapshot                  []byte
	}
	var proofs []proof
	for rows.Next() {
		var p proof
		if err := rows.Scan(&p.action, &p.bot, &p.user, &p.fingerprint, &p.mode, &p.status, &p.snapshot); err != nil {
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
	var created []LinkedAction
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
		var source Snapshot
		if err := json.Unmarshal(p.snapshot, &source); err != nil {
			return nil, err
		}
		for _, actionKind := range []string{"delete", "ban"} {
			var linked int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM bot_invocation_actions l JOIN actions a ON a.id=l.target_action_id WHERE l.source_action_id=? AND a.revision_id=? AND a.kind=?`, p.action, j.ID, actionKind).Scan(&linked); err != nil {
				return nil, err
			}
			if linked > 0 {
				continue
			}
			id, planned, err := planBotActionTx(ctx, tx, j, "bot_invocation", p.bot, 0, p.action, mode, actionKind, &source.Policy)
			if err != nil {
				return nil, err
			}
			if id == 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO bot_invocation_actions(source_action_id,target_action_id) VALUES(?,?)`, p.action, id); err != nil {
				return nil, err
			}
			// Retain the original delete reference for old audit consumers.
			if actionKind == "delete" {
				if _, err := tx.ExecContext(ctx, `UPDATE bot_invocations SET target_action_id=? WHERE source_action_id=?`, id, p.action); err != nil {
					return nil, err
				}
			}
			if planned != nil {
				created = append(created, LinkedAction{ActionID: id, RevisionID: j.ID, SourceActionID: p.action, MessageID: j.MessageID, Kind: actionKind})
			}
		}
	}
	return created, nil
}

func rememberInvocationTx(ctx context.Context, tx *sql.Tx, sourceAction int64, j Job) ([]LinkedAction, error) {
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
	if n > 0 {
		var revision int64
		var kind, mode, evidence string
		if err := tx.QueryRowContext(ctx, `SELECT a.revision_id,a.kind,a.mode,coalesce(e.kind,'') FROM actions a LEFT JOIN action_evidence e ON e.action_id=a.id WHERE a.id=?`, id).Scan(&revision, &kind, &mode, &evidence); err != nil {
			return effects, err
		}
		if evidence != "" {
			if err := auditBotActionTx(ctx, tx, id, "outcome"); err != nil {
				return BotEffects{}, err
			}
		}
		if status == "succeeded" && kind == "delete" && mode == "enforce" {
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
				effects.LinkedActions, err = rememberInvocationTx(ctx, tx, id, *j)
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

type invocationChoice struct{ action, bot int64 }

func (s *Store) blacklistGenerationCurrent(ctx context.Context, bot, generation int64) (bool, error) {
	entry, err := s.BotBlacklist(ctx, bot)
	return entry != nil && entry.Active && entry.Generation == generation, err
}

func (s *Store) checkBotAction(ctx context.Context, a Action) (bool, string, invocationChoice, error) {
	var none invocationChoice
	switch a.EvidenceKind {
	case "":
		return true, "", none, nil
	case "bot_blacklist":
		valid, err := s.blacklistGenerationCurrent(ctx, a.EvidenceBotID, a.EvidenceGeneration)
		return valid, "blacklist entry changed", none, err
	case "bot_invocation":
		j, err := s.Revision(ctx, a.RevisionID)
		if err != nil {
			return false, "", none, err
		}
		var target telegram.Message
		if err := json.Unmarshal(j.Raw, &target); err != nil {
			return false, "", none, err
		}
		if !invocationTargetEligible(*j, &target) {
			return false, "invocation target changed", none, nil
		}
		rows, err := s.db.QueryContext(ctx, `SELECT i.source_action_id,i.bot_id,i.user_id,i.fingerprint,source.mode,source.status,coalesce(e.kind,''),coalesce(e.bot_id,0),coalesce(e.generation,0)
 FROM bot_invocation_actions l JOIN bot_invocations i ON i.source_action_id=l.source_action_id
 JOIN actions source ON source.id=i.source_action_id LEFT JOIN action_evidence e ON e.action_id=source.id
 WHERE l.target_action_id=? ORDER BY i.source_action_id DESC`, a.ID)
		if err != nil {
			return false, "", none, err
		}
		type proof struct {
			action, bot, user, sourceBot, generation int64
			fingerprint, mode, status, kind          string
		}
		var proofs []proof
		for rows.Next() {
			var p proof
			if err := rows.Scan(&p.action, &p.bot, &p.user, &p.fingerprint, &p.mode, &p.status, &p.kind, &p.sourceBot, &p.generation); err != nil {
				rows.Close()
				return false, "", none, err
			}
			proofs = append(proofs, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, "", none, err
		}
		fingerprint := target.InvocationFingerprint()
		for _, p := range proofs {
			if p.user != a.UserID || p.fingerprint != fingerprint || p.mode != "enforce" || p.status != "succeeded" {
				continue
			}
			if p.kind == "bot_blacklist" {
				if p.sourceBot != p.bot {
					continue
				}
				valid, err := s.blacklistGenerationCurrent(ctx, p.sourceBot, p.generation)
				if err != nil {
					return false, "", none, err
				}
				if !valid {
					continue
				}
			} else if p.kind != "" {
				continue
			}
			return true, "", invocationChoice{p.action, p.bot}, nil
		}
		return false, "no current confirmed invocation evidence", none, nil
	default:
		return false, "unsupported action evidence", none, nil
	}
}

func (s *Store) BotActionCurrent(ctx context.Context, a Action) (bool, string, error) {
	valid, reason, _, err := s.checkBotAction(ctx, a)
	return valid, reason, err
}

// GuardBotAction is acquired after network preflight and held through dispatch
// and outcome persistence. Owner list changes serialize at this boundary.
func (s *Store) GuardBotAction(ctx context.Context, a Action) (func(), bool, string, error) {
	if a.EvidenceKind == "" {
		return func() {}, true, "", nil
	}
	s.botDispatchMu.RLock()
	fail := func(reason string, err error) (func(), bool, string, error) {
		s.botDispatchMu.RUnlock()
		return nil, false, reason, err
	}
	current, err := s.ActionCurrent(ctx, a)
	if err != nil {
		return fail("", err)
	}
	if !current {
		return fail("message has a newer version", nil)
	}
	valid, reason, choice, err := s.checkBotAction(ctx, a)
	if err != nil || !valid {
		return fail(reason, err)
	}
	if choice.action > 0 && choice.action != a.SourceActionID {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fail("", err)
		}
		if err := auditBotActionTx(ctx, tx, a.ID, "before_selection"); err != nil {
			tx.Rollback()
			return fail("", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE action_evidence SET source_action_id=?,bot_id=? WHERE action_id=?`, choice.action, choice.bot, a.ID); err != nil {
			tx.Rollback()
			return fail("", err)
		}
		if err := auditBotActionTx(ctx, tx, a.ID, "selected_evidence"); err != nil {
			tx.Rollback()
			return fail("", err)
		}
		if err := tx.Commit(); err != nil {
			return fail("", err)
		}
	}
	return s.botDispatchMu.RUnlock, true, "", nil
}
