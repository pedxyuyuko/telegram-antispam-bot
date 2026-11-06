package telegram

import (
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

type Reference struct{ Key, Username, URL string }
type ReferenceProfile struct {
	Username string `json:"username,omitempty"`
	Name     string `json:"name,omitempty"`
	Bio      string `json:"bio,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

var referenceUsername = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,31}$`)
var referenceTokens = regexp.MustCompile(`(?i)(?:https?://)?(?:t\.me|telegram\.me)/[^\s<>"']+|@[A-Za-z][A-Za-z0-9_]{0,31}`)

// ParseReference accepts Telegram usernames and public chat/message links.
// Invite links remain explicit unsupported references for Bot API-only lookup.
func ParseReference(text string) (Reference, bool) {
	ref := Reference{}
	if strings.HasPrefix(text, "@") {
		name := strings.TrimPrefix(text, "@")
		if !referenceUsername.MatchString(name) {
			return ref, false
		}
		ref.Username = strings.ToLower(name)
		ref.Key = "username:" + ref.Username
		ref.URL = "https://t.me/" + ref.Username
		return ref, true
	}
	input := text
	if !strings.Contains(input, "://") {
		input = "https://" + input
	}
	u, err := url.Parse(input)
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ref, false
	}
	host := strings.ToLower(u.Hostname())
	if (host != "t.me" && host != "telegram.me") || (u.Port() != "" && u.Port() != "443") {
		return ref, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return ref, false
	}
	if strings.HasPrefix(parts[0], "+") || (parts[0] == "joinchat" && len(parts) > 1) {
		ref.URL = "https://t.me" + u.EscapedPath()
		ref.Key = "invite:" + ref.URL
		return ref, true
	}
	if parts[0] == "s" && len(parts) > 1 {
		parts = parts[1:]
	}
	switch strings.ToLower(parts[0]) {
	case "c", "s", "joinchat", "share", "addstickers", "addemoji", "setlanguage", "proxy", "socks", "login", "iv", "boost", "gift", "invoice", "m":
		return ref, false
	}
	if !referenceUsername.MatchString(parts[0]) {
		return ref, false
	}
	ref.Username = strings.ToLower(parts[0])
	ref.Key = "username:" + ref.Username
	ref.URL = text
	return ref, true
}

type messageSpan struct {
	start, end int
	kind, url  string
}

func textSpans(text string, entities []Entity) []messageSpan {
	boundaries := map[int]int{0: 0}
	units := 0
	for pos, r := range text {
		boundaries[units] = pos
		units++
		if r > 0xffff {
			units++
		}
	}
	boundaries[units] = len(text)
	var spans []messageSpan
	add := func(s messageSpan) {
		if s.start < 0 || s.end > s.start+len(text) || s.end > len(text) || s.end <= s.start {
			return
		}
		for _, old := range spans {
			if s.start < old.end && old.start < s.end {
				return
			}
		}
		spans = append(spans, s)
	}
	for _, entity := range entities {
		start, ok := boundaries[entity.Offset]
		end, okEnd := boundaries[entity.Offset+entity.Length]
		if !ok || !okEnd || end <= start {
			continue
		}
		s := messageSpan{start: start, end: end}
		switch entity.Type {
		case "mention":
			if _, ok := ParseReference(text[start:end]); !ok {
				continue
			}
			s.kind = "mention"
		case "url":
			s.kind = "link"
			s.url = text[start:end]
		case "text_link":
			s.kind = "link"
			s.url = entity.URL
		default:
			continue
		}
		add(s)
	}
	for _, match := range referenceTokens.FindAllStringIndex(text, -1) {
		start, end := match[0], match[1]
		if start > 0 {
			previous := text[start-1]
			if previous >= 'A' && previous <= 'Z' || previous >= 'a' && previous <= 'z' || previous >= '0' && previous <= '9' || strings.ContainsRune("_@./:", rune(previous)) {
				continue
			}
		}
		value := strings.TrimRight(text[start:end], ".,;:!?，。；！？)]}）】")
		end = start + len(value)
		if strings.HasPrefix(value, "@") && end < len(text) {
			next := text[end]
			if next >= 'A' && next <= 'Z' || next >= 'a' && next <= 'z' || next >= '0' && next <= '9' || next == '_' {
				continue
			}
		}
		if _, ok := ParseReference(value); !ok {
			continue
		}
		s := messageSpan{start: start, end: end, kind: "link", url: value}
		if strings.HasPrefix(value, "@") {
			s.kind = "mention"
			s.url = ""
		}
		add(s)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	return spans
}

func ReferencesInText(text string) []Reference {
	var refs []Reference
	seen := map[string]bool{}
	for _, span := range textSpans(text, nil) {
		value := span.url
		if value == "" {
			value = text[span.start:span.end]
		}
		if ref, ok := ParseReference(value); ok && !seen[ref.Key] {
			refs = append(refs, ref)
			seen[ref.Key] = true
		}
	}
	return refs
}

func (m *Message) References(profile Profile) []Reference {
	var refs []Reference
	seen := map[string]bool{}
	add := func(value string) {
		if ref, ok := ParseReference(value); ok && !seen[ref.Key] {
			refs = append(refs, ref)
			seen[ref.Key] = true
		}
	}
	for _, part := range []struct {
		text     string
		entities []Entity
	}{{m.Text, m.Entities}, {m.Caption, m.CaptionEntities}} {
		for _, span := range textSpans(part.text, part.entities) {
			value := span.url
			if value == "" {
				value = part.text[span.start:span.end]
			}
			add(value)
		}
	}
	if body, err := m.contextBody(); err == nil {
		if buttons, ok := body["buttons"].([]contextButton); ok {
			links, _ := body["links"].(map[string]string)
			for _, button := range buttons {
				add(links[button.Link])
			}
		}
	}
	for _, text := range []string{profile.Bio, profile.Description} {
		for _, ref := range ReferencesInText(text) {
			add(ref.URL)
		}
	}
	return refs
}

func profileMaterial(ref Reference, profiles map[string]ReferenceProfile, depth int) map[string]any {
	p, ok := profiles[ref.Key]
	if !ok {
		return nil
	}
	raw, _ := json.Marshal(p)
	var value map[string]any
	_ = json.Unmarshal(raw, &value)
	if depth > 0 {
		var links []map[string]any
		for _, nested := range ReferencesInText(p.Bio) {
			links = append(links, map[string]any{"url": nested.URL, "profile": profileMaterial(nested, profiles, depth-1)})
		}
		if len(links) > 0 {
			value["links"] = links
		}
	}
	return value
}
