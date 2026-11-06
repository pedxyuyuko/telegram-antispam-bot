package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/go-telegram/bot"
)

type ReferenceLookup struct {
	Reference  Reference        `json:"reference"`
	Profile    ReferenceProfile `json:"profile"`
	ChatID     int64            `json:"chat_id"`
	Status     string           `json:"status"`
	Error      string           `json:"error,omitempty"`
	Request    json.RawMessage  `json:"request"`
	Response   json.RawMessage  `json:"response"`
	FetchedAt  int64            `json:"fetched_at"`
	DurationMS int64            `json:"duration_ms"`
}

// LookupReference uses only the existing Bot API connection. Known bot IDs may
// resolve private profiles; arbitrary usernames are queried as public chats.
func (c *Client) LookupReference(ctx context.Context, ref Reference, knownID int64) (result ReferenceLookup) {
	started := time.Now()
	result.Reference = ref
	result.FetchedAt = started.UnixMilli()
	result.Status = "unavailable"
	defer func() { result.DurationMS = time.Since(started).Milliseconds() }()
	if ref.Username == "" || !referenceUsername.MatchString(ref.Username) {
		result.Status = "unsupported"
		result.Error = "Bot API lookup requires a public username or an observed bot ID"
		return result
	}
	var target any = "@" + ref.Username
	if knownID > 0 {
		target = knownID
	}
	result.Request, _ = json.Marshal(map[string]any{"chat_id": target})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	chat, err := c.Bot.GetChat(ctx, &bot.GetChatParams{ChatID: target})
	if err != nil {
		result.Error = c.safe(err).Error()
		return result
	}
	if chat == nil {
		result.Error = "Telegram returned an empty reference profile"
		return result
	}
	result.Response, _ = json.Marshal(chat)
	result.ChatID = chat.ID
	if chat.ID == 0 || !strings.EqualFold(chat.Username, ref.Username) {
		result.Error = "returned username does not match the referenced identity"
		return result
	}
	result.Profile = ReferenceProfile{Username: chat.Username, Name: chat.Title, Bio: chat.Description, Kind: string(chat.Type)}
	if result.Profile.Name == "" {
		result.Profile.Name = strings.TrimSpace(chat.FirstName + " " + chat.LastName)
	}
	if result.Profile.Bio == "" {
		result.Profile.Bio = chat.Bio
	}
	result.Status = "available"
	return result
}
