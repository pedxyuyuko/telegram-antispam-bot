package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/go-telegram/bot"
)

type MemberState struct {
	User     User   `json:"user"`
	Status   string `json:"status"`
	IsMember bool   `json:"is_member,omitempty"`
}

func (m MemberState) Present() bool {
	return m.Status == "member" || m.Status == "administrator" || m.Status == "creator" || (m.Status == "restricted" && m.IsMember)
}

type MembershipUpdate struct {
	Chat Chat        `json:"chat"`
	From User        `json:"from"`
	Date int64       `json:"date"`
	Old  MemberState `json:"old_chat_member"`
	New  MemberState `json:"new_chat_member"`
}

func (m MembershipUpdate) Valid() bool {
	validStatus := func(s string) bool {
		switch s {
		case "member", "administrator", "creator", "restricted", "left", "kicked":
			return true
		}
		return false
	}
	return m.Chat.ID < 0 && (m.Chat.Type == "group" || m.Chat.Type == "supergroup") && m.Date > 0 && m.New.User.ID > 0 && m.New.User.ID == m.Old.User.ID && validStatus(m.Old.Status) && validStatus(m.New.Status)
}

func (m MembershipUpdate) Joined() bool { return !m.Old.Present() && m.New.Present() }

type JoinProfileLookup struct {
	Profile
	Request    json.RawMessage `json:"request"`
	Response   json.RawMessage `json:"response"`
	Error      string          `json:"error,omitempty"`
	DurationMS int64           `json:"duration_ms"`
}

// JoinProfile reads fresh biography material for this arrival. A new arrival
// obtains a new snapshot; inference retries use the persisted snapshot.
func (c *Client) JoinProfile(ctx context.Context, user int64) (result JoinProfileLookup) {
	start := time.Now()
	result.Status = "unavailable"
	result.FetchedAt = start.Unix()
	defer func() { result.DurationMS = time.Since(start).Milliseconds() }()
	result.Request, _ = json.Marshal(map[string]any{"chat_id": user})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	chat, err := c.Bot.GetChat(ctx, &bot.GetChatParams{ChatID: user})
	if err != nil {
		result.Error = c.safe(err).Error()
		return result
	}
	result.Response, _ = json.Marshal(chat)
	if chat == nil || chat.ID != user || chat.Type != "private" {
		result.Error = "profile does not match the joining user"
		return result
	}
	result.Status = "available"
	result.Bio = chat.Bio
	return result
}

func JoinModelState(user User, profile JoinProfileLookup) (json.RawMessage, error) {
	var bio *string
	if profile.Status == "available" && profile.Bio != "" {
		bio = &profile.Bio
	}
	return json.Marshal(map[string]any{"member": map[string]any{"name": strings.TrimSpace(user.FirstName + " " + user.LastName), "bio": bio, "bio_status": profile.Status}})
}
