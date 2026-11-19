package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"telegram-antispam-bot/internal/vision"
)

// Only these optional getChat fields participate in message moderation.
type senderCard struct {
	photo   *models.ChatPhoto
	emoji   string
	expires int
	channel *models.Chat
}

type imageSlot struct {
	source ImageSource
	owner  map[string]any
}

func avatarSlot(owner map[string]any, photo *models.ChatPhoto, kind string) []imageSlot {
	if photo == nil {
		return nil
	}
	id, unique := photo.SmallFileID, photo.SmallFileUniqueID
	if id == "" {
		id, unique = photo.BigFileID, photo.BigFileUniqueID
	}
	if id == "" {
		return nil
	}
	avatar := map[string]any{"type": "photo"}
	owner["avatar"] = avatar
	return []imageSlot{{ImageSource{Kind: kind, FileID: id, FileUniqueID: unique}, avatar}}
}

func (c *Client) personalChannel(ctx context.Context, user int64, channel *models.Chat) (map[string]any, []imageSlot) {
	card := map[string]any{"id": channel.ID, "title": channel.Title}
	if channel.Username != "" {
		card["username"] = channel.Username
	}
	var slots []imageSlot
	// This endpoint takes the user's ID. Verify the returned post belongs to
	// the same channel in case the user has switched their showcase meanwhile.
	posts, err := c.Bot.GetUserPersonalChatMessages(ctx, &bot.GetUserPersonalChatMessagesParams{UserID: user, Limit: 1})
	if err == nil && len(posts) > 0 && posts[0].Chat.ID == channel.ID {
		p := posts[0]
		post := map[string]any{"message_id": p.ID, "date": p.Date}
		card["latest_post"] = post
		if p.Text != "" {
			post["text"] = p.Text
		}
		if p.Caption != "" {
			post["caption"] = p.Caption
		}
		if len(p.Photo) > 0 {
			raw, _ := json.Marshal(map[string]any{"photo": p.Photo})
			src := (&Message{Raw: raw}).ImageSources()[0]
			src.Kind = "channel_post_photo"
			slots = append(slots, imageSlot{src, post})
		}
	}
	if n, err := c.Bot.GetChatMemberCount(ctx, &bot.GetChatMemberCountParams{ChatID: channel.ID}); err == nil {
		card["subscribers"] = n
	}
	if full, err := c.Bot.GetChat(ctx, &bot.GetChatParams{ChatID: channel.ID}); err == nil && full != nil && full.ID == channel.ID {
		if full.Description != "" {
			card["description"] = full.Description
		}
		slots = append(slots, avatarSlot(card, full.Photo, "channel_avatar")...)
	}
	return card, slots
}

// One title lookup per distinct sticker pack per revision. StickerSet has
// a short name and display title; Telegram supplies no description field.
type stickerTitles map[string]string

func (t stickerTitles) decorate(ctx context.Context, c *Client, owner map[string]any, s models.Sticker) {
	if s.Type != "" {
		owner["sticker_type"] = s.Type
	}
	if s.Emoji != "" {
		owner["emoji"] = s.Emoji
	}
	if s.SetName == "" {
		return
	}
	owner["set_name"] = s.SetName
	title, ok := t[s.SetName]
	if !ok {
		if set, err := c.Bot.GetStickerSet(ctx, &bot.GetStickerSetParams{Name: s.SetName}); err == nil && set != nil && strings.EqualFold(set.Name, s.SetName) {
			title = set.Title
		}
		t[s.SetName] = title
	}
	if title != "" {
		owner["set_title"] = title
	}
}

type emojiSlot struct {
	id, kind string
	owner    map[string]any
}

const maxBodyEmoji = 3

func bodyEmojiSlots(m *Message, root map[string]any) []emojiSlot {
	var out []emojiSlot
	parts, _ := root["message"].([]any)
	for _, src := range []struct {
		kind, text string
		entities   []Entity
	}{{"text", m.Text, m.Entities}, {"caption", m.Caption, m.CaptionEntities}} {
		for _, e := range src.entities {
			if e.Type != "custom_emoji" || e.CustomEmojiID == "" {
				continue
			}
			text := EntityText(src.text, e)
			if text == "" {
				continue
			}
			owner := map[string]any{"type": "custom_emoji", "custom_emoji_id": e.CustomEmojiID, "source": src.kind, "text": text, "offset": e.Offset, "length": e.Length}
			parts = append(parts, owner)
			root["message"] = parts
			out = append(out, emojiSlot{e.CustomEmojiID, "message_emoji", owner})
			if len(out) == maxBodyEmoji {
				return out
			}
		}
	}
	return out
}

func (c *Client) resolveEmojis(ctx context.Context, targets []emojiSlot, titles stickerTitles) []imageSlot {
	if len(targets) == 0 {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	for _, t := range targets {
		if !seen[t.id] {
			ids = append(ids, t.id)
			seen[t.id] = true
		}
	}
	stickers, err := c.Bot.GetCustomEmojiStickers(ctx, &bot.GetCustomEmojiStickersParams{CustomEmojiIDs: ids})
	if err != nil {
		return nil
	}
	var slots []imageSlot
	for _, t := range targets {
		for _, s := range stickers {
			if s == nil || s.CustomEmojiID != t.id {
				continue
			}
			titles.decorate(ctx, c, t.owner, *s)
			raw, _ := json.Marshal(map[string]any{"sticker": s})
			src := (&Message{Raw: raw}).ImageSources()[0]
			src.Kind = t.kind
			slots = append(slots, imageSlot{src, t.owner})
			break
		}
	}
	return slots
}

// RichContext builds a frozen, source-scoped visual request. Optional Bot API
// misses leave the rest of the material available for the same inference.
func (c *Client) RichContext(ctx context.Context, m *Message, profile Profile, state json.RawMessage) (json.RawMessage, vision.Assets, []ImageLookup, error) {
	root, media, err := currentMedia(state)
	if err != nil {
		return nil, nil, nil, err
	}
	sender, ok := root["sender"].(map[string]any)
	if !ok {
		return nil, nil, nil, fmt.Errorf("missing segmented sender")
	}
	meta, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var slots, cardSlots []imageSlot
	titles := stickerTitles{}
	var current struct {
		Sticker *models.Sticker `json:"sticker"`
	}
	_ = json.Unmarshal(m.Bytes(), &current)
	for _, src := range m.ImageSources() {
		for _, owner := range media {
			if owner["type"] != src.Kind {
				continue
			}
			slots = append(slots, imageSlot{src, owner})
			if src.Kind == "sticker" && current.Sticker != nil {
				titles.decorate(meta, c, owner, *current.Sticker)
			}
			break
		}
	}
	emojis := bodyEmojiSlots(m, root)
	if id := m.UserID(); id != 0 {
		sender["id"] = id
		sender["is_premium"] = m.From.IsPremium
		if p := profile.card; p != nil {
			if p.emoji != "" && (p.expires == 0 || int64(p.expires) > time.Now().Unix()) {
				owner := map[string]any{"custom_emoji_id": p.emoji}
				sender["emoji_status"] = owner
				emojis = append([]emojiSlot{{p.emoji, "status_emoji", owner}}, emojis...)
			}
			if ch := p.channel; ch != nil && ch.ID != 0 && ch.Type == models.ChatTypeChannel {
				card, images := c.personalChannel(meta, id, ch)
				sender["personal_channel"] = card
				cardSlots = append(cardSlots, images...)
			}
			cardSlots = append(cardSlots, avatarSlot(sender, p.photo, "sender_avatar")...)
		}
	}
	slots = append(slots, c.resolveEmojis(meta, emojis, titles)...)
	slots = append(slots, cardSlots...)
	material, err := marshalStateOrder(state, root)
	if err != nil {
		return nil, nil, nil, err
	}
	assets := vision.Assets{}
	var lookups []ImageLookup
	download, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	for i, slot := range slots {
		l := ImageLookup{Source: slot.source, Status: "limit", Error: "per-message image budget exhausted", FetchedAt: time.Now().UnixMilli()}
		if i < 6 && download.Err() == nil {
			l = c.LookupImage(download, slot.source)
		}
		if l.Status == "available" {
			if err := attachLookup(material, assets, slot.owner, &l); err != nil {
				return nil, nil, nil, err
			}
		}
		lookups = append(lookups, l)
	}
	raw, err := marshalStateOrder(material, root)
	return raw, assets, lookups, err
}
