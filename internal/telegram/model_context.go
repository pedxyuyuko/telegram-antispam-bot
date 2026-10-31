package telegram

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type contextLink struct {
	Text string `json:"text"`
	Link string `json:"link"`
}

type contextButton struct {
	Text  string `json:"text"`
	Link  string `json:"link,omitempty"`
	Count int    `json:"count"`
}

// ModelState separates the current author, quoted message and current message.
// Bytes remains the full original archive; no field is removed from that archive.
func (m *Message) ModelState(profile Profile) (json.RawMessage, error) {
	sender := map[string]any{"name": nil, "username": nil, "is_bot": nil, "bio": nil}
	if m.From != nil {
		sender["name"] = strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
		if m.From.Username != "" {
			sender["username"] = m.From.Username
		}
		sender["is_bot"] = m.From.IsBot
	}
	if m.SenderChat != nil {
		var source struct {
			SenderChat struct {
				Username string `json:"username"`
			} `json:"sender_chat"`
		}
		if err := json.Unmarshal(m.Bytes(), &source); err != nil {
			return nil, err
		}
		sender["name"] = m.SenderChat.Title
		sender["username"] = nil
		sender["is_bot"] = nil
		if source.SenderChat.Username != "" {
			sender["username"] = source.SenderChat.Username
		}
	}
	if profile.Bio != "" {
		sender["bio"] = profile.Bio
	}
	if profile.Description != "" {
		sender["description"] = profile.Description
	}
	msg, err := m.contextBody()
	if err != nil {
		return nil, err
	}
	var reply map[string]any
	if m.Reply != nil {
		reply, err = m.Reply.contextBody()
		if err != nil {
			return nil, fmt.Errorf("reply: %w", err)
		}
		reply["sender"] = contextSender(m.Reply)
	}
	return json.Marshal(struct {
		Sender map[string]any `json:"sender"`
		Reply  map[string]any `json:"reply"`
		Msg    map[string]any `json:"msg"`
	}{sender, reply, msg})
}

func contextUser(user *User) string {
	if user == nil {
		return "（未提供）"
	}
	name := strings.TrimSpace(user.FirstName + " " + user.LastName)
	if user.Username != "" {
		name += " (@" + user.Username + ")"
	}
	return name
}

func contextSender(m *Message) string {
	if m.SenderChat != nil && m.SenderChat.Title != "" {
		return m.SenderChat.Title
	}
	return contextUser(m.From)
}

func (m *Message) contextBody() (map[string]any, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(m.Bytes(), &fields); err != nil {
		return nil, err
	}
	body := map[string]any{"text": m.Text}
	if m.Caption != "" {
		body["caption"] = m.Caption
	}
	links := map[string]string{}
	linkIDs := map[string]string{}
	addLink := func(url string) string {
		if url == "" {
			return ""
		}
		if id, ok := linkIDs[url]; ok {
			return id
		}
		id := strconv.Itoa(len(links) + 1)
		links[id] = url
		linkIDs[url] = id
		return id
	}
	for _, part := range []struct {
		Key, Text string
		Entities  []Entity
	}{
		{"text_links", m.Text, m.Entities}, {"caption_links", m.Caption, m.CaptionEntities},
	} {
		var expanded []contextLink
		seen := map[[2]string]bool{}
		for _, entity := range part.Entities {
			if entity.URL == "" {
				continue
			}
			text := EntityText(part.Text, entity)
			key := [2]string{text, entity.URL}
			if seen[key] {
				continue
			}
			seen[key] = true
			expanded = append(expanded, contextLink{text, addLink(entity.URL)})
		}
		if len(expanded) > 0 {
			body[part.Key] = expanded
		}
	}
	var markup struct {
		Rows [][]struct {
			Text   string `json:"text"`
			URL    string `json:"url"`
			WebApp *struct {
				URL string `json:"url"`
			} `json:"web_app"`
		} `json:"inline_keyboard"`
	}
	if raw := fields["reply_markup"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &markup); err != nil {
			return nil, fmt.Errorf("buttons: %w", err)
		}
	}
	var buttons []contextButton
	indices := map[[2]string]int{}
	for _, row := range markup.Rows {
		for _, button := range row {
			url := button.URL
			if url == "" && button.WebApp != nil {
				url = button.WebApp.URL
			}
			key := [2]string{button.Text, url}
			if i, ok := indices[key]; ok {
				buttons[i].Count++
				continue
			}
			indices[key] = len(buttons)
			buttons = append(buttons, contextButton{button.Text, addLink(url), 1})
		}
	}
	if len(buttons) > 0 {
		body["buttons"] = buttons
	}
	if len(links) > 0 {
		body["links"] = links
	}
	media, err := contextMedia(fields)
	if err != nil {
		return nil, err
	}
	if len(media) > 0 {
		body["media"] = media
	}
	for _, source := range []struct{ Field, Key string }{{"via_bot", "via_bot"}, {"guest_bot_caller_user", "caller"}} {
		if raw := fields[source.Field]; len(raw) > 0 {
			var user User
			if err := json.Unmarshal(raw, &user); err != nil {
				return nil, fmt.Errorf("%s: %w", source.Field, err)
			}
			body[source.Key] = contextUser(&user)
		}
	}
	if raw := fields["forward_origin"]; len(raw) > 0 {
		type originChat struct {
			Title    string `json:"title"`
			Username string `json:"username"`
		}
		var origin struct {
			Chat       *originChat `json:"chat"`
			SenderChat *originChat `json:"sender_chat"`
			User       *User       `json:"sender_user"`
			HiddenName string      `json:"sender_user_name"`
		}
		if err := json.Unmarshal(raw, &origin); err != nil {
			return nil, fmt.Errorf("forward origin: %w", err)
		}
		if origin.SenderChat != nil {
			origin.Chat = origin.SenderChat
		}
		var name string
		if origin.Chat != nil {
			name = origin.Chat.Title
			if origin.Chat.Username != "" {
				name += " (@" + origin.Chat.Username + ")"
			}
		} else if origin.User != nil {
			name = contextUser(origin.User)
		} else {
			name = origin.HiddenName
		}
		if name != "" {
			body["forwarded_from"] = name
		}
	}
	return body, nil
}

func contextMedia(fields map[string]json.RawMessage) ([]map[string]any, error) {
	var attachments []map[string]any
	for _, kind := range []string{"audio", "voice", "video", "animation", "document", "sticker"} {
		raw := fields[kind]
		if len(raw) == 0 {
			continue
		}
		var media map[string]json.RawMessage
		if err := json.Unmarshal(raw, &media); err != nil {
			return nil, fmt.Errorf("%s: %w", kind, err)
		}
		item := map[string]any{"type": kind}
		for _, key := range []string{"file_name", "title", "performer", "mime_type", "emoji", "set_name"} {
			if value, ok := media[key]; ok {
				var text string
				if err := json.Unmarshal(value, &text); err != nil {
					return nil, fmt.Errorf("%s.%s: %w", kind, key, err)
				}
				if text != "" {
					item[key] = text
				}
			}
		}
		if value, ok := media["duration"]; ok {
			var duration int
			if err := json.Unmarshal(value, &duration); err != nil {
				return nil, fmt.Errorf("%s.duration: %w", kind, err)
			}
			if duration > 0 {
				item["duration"] = duration
			}
		}
		attachments = append(attachments, item)
	}
	if len(fields["photo"]) > 0 {
		attachments = append(attachments, map[string]any{"type": "photo"})
	}
	return attachments, nil
}
