package telegram

import "encoding/json"

const SegmentedContextVersion = 2

func segmentText(text string, entities []Entity, source string, profiles map[string]ReferenceProfile) []map[string]any {
	parts := make([]map[string]any, 0)
	appendPart := func(part map[string]any) {
		if source != "text" {
			part["source"] = source
		}
		parts = append(parts, part)
	}
	pos := 0
	for _, span := range textSpans(text, entities) {
		if span.start > pos {
			appendPart(map[string]any{"type": "text", "text": text[pos:span.start]})
		}
		part := map[string]any{"type": span.kind, "text": text[span.start:span.end]}
		target := span.url
		depth := 0
		if span.kind == "mention" {
			target = text[span.start:span.end]
			depth = 1
		} else {
			part["url"] = span.url
		}
		if ref, ok := ParseReference(target); ok {
			part["profile"] = profileMaterial(ref, profiles, depth)
		}
		appendPart(part)
		pos = span.end
	}
	if pos < len(text) {
		appendPart(map[string]any{"type": "text", "text": text[pos:]})
	}
	return parts
}

// SegmentedModelState keeps author and reply material separate while attaching
// fetched target profiles to the exact mention/link in the current message.
func (m *Message) SegmentedModelState(profile Profile, profiles map[string]ReferenceProfile) (json.RawMessage, error) {
	legacy, err := m.ModelState(profile)
	if err != nil {
		return nil, err
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(legacy, &state); err != nil {
		return nil, err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(state["msg"], &body); err != nil {
		return nil, err
	}
	parts := segmentText(m.Text, m.Entities, "text", profiles)
	parts = append(parts, segmentText(m.Caption, m.CaptionEntities, "caption", profiles)...)
	if buttons, ok := body["buttons"]; ok {
		part := map[string]any{"type": "buttons", "buttons": buttons}
		var links map[string]string
		if raw, exists := body["links"]; exists {
			if err := json.Unmarshal(raw, &links); err != nil {
				return nil, err
			}
			part["links"] = raw
		}
		resolved := map[string]any{}
		for id, target := range links {
			if ref, ok := ParseReference(target); ok {
				resolved[id] = profileMaterial(ref, profiles, 0)
			}
		}
		if len(resolved) > 0 {
			part["profiles"] = resolved
		}
		parts = append(parts, part)
	}
	if media, ok := body["media"]; ok {
		parts = append(parts, map[string]any{"type": "media", "media": media})
	}
	source := map[string]any{"type": "source"}
	for _, key := range []string{"via_bot", "caller", "forwarded_from"} {
		if value, ok := body[key]; ok {
			source[key] = value
		}
	}
	if len(source) > 1 {
		parts = append(parts, source)
	}
	var sender map[string]any
	if err := json.Unmarshal(state["sender"], &sender); err != nil {
		return nil, err
	}
	var links []map[string]any
	for _, text := range []string{profile.Bio, profile.Description} {
		for _, ref := range ReferencesInText(text) {
			links = append(links, map[string]any{"url": ref.URL, "profile": profileMaterial(ref, profiles, 0)})
		}
	}
	if len(links) > 0 {
		sender["links"] = links
	}
	return json.Marshal(struct {
		Sender  map[string]any   `json:"sender"`
		Reply   json.RawMessage  `json:"reply"`
		Message []map[string]any `json:"message"`
	}{sender, state["reply"], parts})
}
