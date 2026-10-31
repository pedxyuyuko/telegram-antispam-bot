package telegram

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func compactFixture(t *testing.T, raw string, profile Profile) (map[string]any, *Message) {
	t.Helper()
	var message Message
	if err := json.Unmarshal([]byte(raw), &message); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), message.Bytes()...)
	state, err := message.ModelState(profile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, message.Bytes()) {
		t.Fatal("raw archive changed during context construction")
	}
	var context map[string]any
	if err := json.Unmarshal(state, &context); err != nil {
		t.Fatal(err)
	}
	if len(context) != 3 {
		t.Fatalf("expected three source sections, got %v", context)
	}
	for _, key := range []string{"sender", "reply", "msg"} {
		if _, ok := context[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	return context, &message
}

func TestModelContextSeparatesSenderReplyAndCurrentMessage(t *testing.T) {
	c, m := compactFixture(t, `{"message_id":1,"date":123456,"chat":{"id":-100,"type":"supergroup"},"from":{"id":42,"first_name":"Alice","username":"alice","is_bot":true},"text":"CURRENT_ONLY","reply_to_message":{"message_id":2,"from":{"id":8,"first_name":"Bob"},"text":"QUOTED_ONLY","reply_to_message":{"text":"DEEP_QUOTE"}},"future_field":"ARCHIVE_ONLY"}`, Profile{Status: "available", Bio: "PROFILE_ONLY", FetchedAt: 123456})
	if c["sender"].(map[string]any)["bio"] != "PROFILE_ONLY" || c["sender"].(map[string]any)["is_bot"] != true {
		t.Fatal("sender profile lost")
	}
	if c["msg"].(map[string]any)["text"] != "CURRENT_ONLY" || c["reply"].(map[string]any)["text"] != "QUOTED_ONLY" || c["reply"].(map[string]any)["sender"] != "Bob" {
		t.Fatal("message sources crossed")
	}
	raw, _ := json.Marshal(c)
	for _, noise := range []string{"message_id", "123456", "ARCHIVE_ONLY", "DEEP_QUOTE"} {
		if strings.Contains(string(raw), noise) {
			t.Errorf("unneeded metadata %s in context", noise)
		}
	}
	if !strings.Contains(string(m.Bytes()), "ARCHIVE_ONLY") || !strings.Contains(string(m.Bytes()), "DEEP_QUOTE") {
		t.Fatal("archive lost original fields")
	}
}

func TestModelContextDeduplicatesButtonsAndScopesLinks(t *testing.T) {
	c, _ := compactFixture(t, `{"message_id":1,"chat":{"id":-100,"type":"supergroup"},"text":"1","reply_markup":{"inline_keyboard":[[{"text":"A","url":"https://example.test/current","style":"danger"}],[{"text":"A","url":"https://example.test/current","style":"success"}],[{"text":"B","url":"https://example.test/current"}]]},"reply_to_message":{"message_id":2,"text":"quoted","reply_markup":{"inline_keyboard":[[{"text":"Quoted","url":"https://example.test/quoted"}]]}}}`, Profile{Status: "unavailable"})
	msg := c["msg"].(map[string]any)
	reply := c["reply"].(map[string]any)
	buttons := msg["buttons"].([]any)
	if len(buttons) != 2 || buttons[0].(map[string]any)["count"] != float64(2) || buttons[1].(map[string]any)["count"] != float64(1) {
		t.Fatal("button counts changed")
	}
	if len(msg["links"].(map[string]any)) != 1 || msg["links"].(map[string]any)["1"] != "https://example.test/current" || reply["links"].(map[string]any)["1"] != "https://example.test/quoted" {
		t.Fatal("current and quoted links crossed")
	}
	if c["sender"].(map[string]any)["bio"] != nil {
		t.Fatal("missing biography invented")
	}
}

func TestModelContextExpandsUTF16EntitiesAndMediaDescriptions(t *testing.T) {
	c, _ := compactFixture(t, `{"message_id":1,"chat":{"id":-100,"type":"supergroup"},"text":"🙂点我领取","entities":[{"type":"text_link","offset":2,"length":4,"url":"https://example.test/claim"},{"type":"bold","offset":2,"length":4}],"caption":"下载","caption_entities":[{"type":"text_link","offset":0,"length":2,"url":"https://example.test/file"}],"audio":{"file_name":"music.flac","duration":120,"title":"Music","performer":"Artist","file_id":"TRANSPORT_ONLY"},"via_bot":{"first_name":"Helper","username":"inline_bot"},"guest_bot_caller_user":{"first_name":"Caller"}}`, Profile{})
	msg := c["msg"].(map[string]any)
	link := msg["text_links"].([]any)[0].(map[string]any)
	if link["text"] != "点我领取" || msg["links"].(map[string]any)[link["link"].(string)] != "https://example.test/claim" {
		t.Fatal("UTF-16 entity was not expanded")
	}
	if msg["caption_links"].([]any)[0].(map[string]any)["text"] != "下载" {
		t.Fatal("caption link lost")
	}
	media := msg["media"].([]any)[0].(map[string]any)
	if media["type"] != "audio" || media["file_name"] != "music.flac" || media["performer"] != "Artist" || media["duration"] != float64(120) {
		t.Fatal("media description lost")
	}
	if msg["via_bot"] != "Helper (@inline_bot)" || msg["caller"] != "Caller" {
		t.Fatal("bot provenance lost")
	}
	if c["reply"] != nil {
		t.Fatal("reply invented")
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "TRANSPORT_ONLY") || strings.Contains(string(raw), "offset") {
		t.Fatal("transport metadata retained")
	}
}

func TestModelContextPreservesChannelIdentityAndDescription(t *testing.T) {
	c, _ := compactFixture(t, `{"message_id":1,"chat":{"id":-100,"type":"supergroup"},"sender_chat":{"id":-200,"title":"Publisher","username":"publisher"},"from":{"id":777000,"first_name":"Relay","is_bot":false},"text":"post","forward_origin":{"type":"channel","chat":{"title":"Original","username":"original"}}}`, Profile{Status: "available", Description: "CHANNEL_DESCRIPTION"})
	sender := c["sender"].(map[string]any)
	if sender["name"] != "Publisher" || sender["username"] != "publisher" || sender["description"] != "CHANNEL_DESCRIPTION" {
		t.Fatal("channel attributed to relay user")
	}
	if c["msg"].(map[string]any)["forwarded_from"] != "Original (@original)" {
		t.Fatal("forward provenance lost")
	}
}

func TestModelContextRejectsMalformedRichContent(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"message_id":1,"chat":{"id":-100,"type":"supergroup"},"reply_markup":{"inline_keyboard":42}}`), &m); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ModelState(Profile{}); err == nil {
		t.Fatal("malformed buttons silently discarded")
	}
}
