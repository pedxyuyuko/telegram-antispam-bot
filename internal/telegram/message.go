package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf16"
)

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot,omitempty"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}
type Chat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
}
type Entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
	URL    string `json:"url,omitempty"`
	User   *User  `json:"user,omitempty"`
}
type Message struct {
	ID              int             `json:"message_id"`
	Chat            Chat            `json:"chat"`
	From            *User           `json:"from,omitempty"`
	SenderChat      *Chat           `json:"sender_chat,omitempty"`
	Date            int64           `json:"date"`
	EditDate        int64           `json:"edit_date,omitempty"`
	Text            string          `json:"text,omitempty"`
	Caption         string          `json:"caption,omitempty"`
	Entities        []Entity        `json:"entities,omitempty"`
	CaptionEntities []Entity        `json:"caption_entities,omitempty"`
	Reply           *Message        `json:"reply_to_message,omitempty"`
	Raw             json.RawMessage `json:"-"`
}

func (m *Message) UnmarshalJSON(raw []byte) error {
	type plain Message
	var p plain
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	*m = Message(p)
	m.Raw = append(json.RawMessage(nil), raw...)
	return nil
}

type Update struct {
	ID      int64    `json:"update_id"`
	Message *Message `json:"message,omitempty"`
	Edited  *Message `json:"edited_message,omitempty"`
}

func Parse(raw []byte) (Update, error) {
	var u Update
	if err := json.Unmarshal(raw, &u); err != nil {
		return u, err
	}
	if u.ID <= 0 {
		return u, errors.New("missing update_id")
	}
	return u, nil
}
func (u Update) Current() *Message {
	if u.Edited != nil {
		return u.Edited
	}
	return u.Message
}
func (m *Message) UserID() int64 {
	if m.SenderChat != nil || m.From == nil {
		return 0
	}
	return m.From.ID
}
func (m *Message) SenderKey() string {
	if m.SenderChat != nil {
		return fmt.Sprintf("chat:%d", m.SenderChat.ID)
	}
	return fmt.Sprintf("user:%d", m.UserID())
}
func (m *Message) IsGroup() bool { return m.Chat.Type == "group" || m.Chat.Type == "supergroup" }
func (m *Message) Bytes() json.RawMessage {
	if len(m.Raw) > 0 {
		return m.Raw
	}
	raw, _ := json.Marshal(m)
	return raw
}

func EntityText(text string, e Entity) string {
	units := utf16.Encode([]rune(text))
	if e.Offset < 0 || e.Length < 0 || e.Offset > len(units) || e.Length > len(units)-e.Offset {
		return ""
	}
	return string(utf16.Decode(units[e.Offset : e.Offset+e.Length]))
}

type Profile struct {
	Status      string `json:"status"`
	Bio         string `json:"bio,omitempty"`
	Description string `json:"description,omitempty"`
	FetchedAt   int64  `json:"fetched_at,omitempty"`
}

func (m *Message) State(profile Profile) json.RawMessage {
	type expanded struct {
		Type string `json:"type"`
		Text string `json:"text"`
		URL  string `json:"url,omitempty"`
		User *User  `json:"user,omitempty"`
	}
	var entities []expanded
	for _, e := range m.Entities {
		entities = append(entities, expanded{e.Type, EntityText(m.Text, e), e.URL, e.User})
	}
	for _, e := range m.CaptionEntities {
		entities = append(entities, expanded{e.Type, EntityText(m.Caption, e), e.URL, e.User})
	}
	raw, _ := json.Marshal(struct {
		Message  json.RawMessage `json:"telegram_message"`
		Profile  Profile         `json:"sender_profile"`
		Entities []expanded      `json:"expanded_entities"`
	}{m.Bytes(), profile, entities})
	return raw
}
