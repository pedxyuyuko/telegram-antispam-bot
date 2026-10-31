package telegram

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

func (m *Message) BotSenderID() int64 {
	if m == nil || m.SenderChat != nil || m.From == nil || !m.From.IsBot || m.From.ID <= 0 {
		return 0
	}
	return m.From.ID
}

// GuestInvocation uses Telegram's caller metadata, never an arbitrary bot reply.
func (m *Message) GuestInvocation() *Message {
	if m == nil || !m.IsGroup() || m.BotSenderID() == 0 || m.IsAutomaticForward {
		return nil
	}
	caller, r := m.GuestBotCallerUser, m.Reply
	if caller == nil || caller.ID <= 0 || caller.IsBot || r == nil || !r.IsGroup() || r.Chat.ID != m.Chat.ID || r.ID <= 0 || r.ID >= m.ID {
		return nil
	}
	if r.SenderChat != nil || r.IsAutomaticForward || r.From == nil || r.From.IsBot || r.From.ID != caller.ID {
		return nil
	}
	return r
}

// InvocationFingerprint compares the caller's current version with the version
// Telegram embedded in the guest reply. Nested replies and display names vary.
func (m *Message) InvocationFingerprint() string {
	type stableEntity struct {
		Type   string `json:"type"`
		Offset int    `json:"offset"`
		Length int    `json:"length"`
		URL    string `json:"url,omitempty"`
		UserID int64  `json:"user_id,omitempty"`
	}
	stable := func(entities []Entity) []stableEntity {
		var out []stableEntity
		for _, entity := range entities {
			var user int64
			if entity.User != nil {
				user = entity.User.ID
			}
			out = append(out, stableEntity{entity.Type, entity.Offset, entity.Length, entity.URL, user})
		}
		return out
	}
	raw, _ := json.Marshal(struct {
		Chat                      int64
		Message                   int
		User                      int64
		Date, EditDate            int64
		Text, Caption             string
		Entities, CaptionEntities []stableEntity
	}{m.Chat.ID, m.ID, m.UserID(), m.Date, m.EditDate, m.Text, m.Caption, stable(m.Entities), stable(m.CaptionEntities)})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
