package telegram

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type ContentFingerprint struct {
	Hash     string
	Material json.RawMessage
}

// ContentFingerprint deliberately separates message content from author/caller
// identity. The original message is untouched. Empty Hash means not cacheable.
func (m *Message) ContentFingerprint() (ContentFingerprint, error) {
	body, err := m.contextBody()
	if err != nil {
		return ContentFingerprint{}, err
	}
	delete(body, "caller")
	delete(body, "via_bot")
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(m.Bytes(), &fields); err != nil {
		return ContentFingerprint{}, err
	}
	identities := map[string][]string{}
	for _, kind := range []string{"photo", "audio", "voice", "video", "animation", "document", "sticker", "video_note"} {
		raw, exists := fields[kind]
		if !exists {
			continue
		}
		type media struct {
			ID string `json:"file_unique_id"`
		}
		var items []media
		if kind == "photo" {
			if err := json.Unmarshal(raw, &items); err != nil {
				return ContentFingerprint{}, err
			}
		} else {
			var item media
			if err := json.Unmarshal(raw, &item); err != nil {
				return ContentFingerprint{}, err
			}
			items = []media{item}
		}
		if len(items) == 0 {
			return ContentFingerprint{}, nil
		}
		for _, item := range items {
			if item.ID == "" {
				return ContentFingerprint{}, nil
			}
			identities[kind] = append(identities[kind], item.ID)
		}
		sort.Strings(identities[kind])
	}
	if strings.TrimSpace(m.Text) == "" && strings.TrimSpace(m.Caption) == "" && body["buttons"] == nil && len(identities) == 0 {
		return ContentFingerprint{}, nil
	}
	kind := "user"
	if m.BotSenderID() > 0 {
		kind = "bot"
	}
	if m.SenderChat != nil {
		kind = "channel"
	}
	// Formatting (especially quotation/code entities) and media unique IDs are
	// retained so a plain ad and a quoted example cannot share a fingerprint.
	material, err := json.Marshal(struct {
		Version         int                 `json:"version"`
		SenderKind      string              `json:"sender_kind"`
		Body            map[string]any      `json:"body"`
		Entities        []Entity            `json:"entities,omitempty"`
		CaptionEntities []Entity            `json:"caption_entities,omitempty"`
		Media           map[string][]string `json:"media_identity,omitempty"`
	}{1, kind, body, m.Entities, m.CaptionEntities, identities})
	if err != nil {
		return ContentFingerprint{}, err
	}
	return ContentFingerprint{Hash: fmt.Sprintf("%x", sha256.Sum256(material)), Material: material}, nil
}
