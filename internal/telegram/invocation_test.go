package telegram

import (
	"encoding/json"
	"testing"
)

func invocationMessage(t *testing.T) *Message {
	t.Helper()
	var m Message
	if err := json.Unmarshal([]byte(`{"message_id":12,"chat":{"id":-100,"type":"supergroup"},"from":{"id":200,"is_bot":true,"first_name":"Guest","username":"guest_bot"},"text":"advertisement","guest_bot_caller_user":{"id":50,"first_name":"Caller"},"reply_to_message":{"message_id":11,"chat":{"id":-100,"type":"supergroup"},"from":{"id":50,"first_name":"Caller"},"date":123,"text":"/start 720 @guest_bot"}}`), &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestGuestInvocationRequiresTelegramCallerAndMatchingReply(t *testing.T) {
	m := invocationMessage(t)
	if caller := m.GuestInvocation(); caller == nil || caller.ID != 11 || caller.UserID() != 50 {
		t.Fatal("trusted invocation lost")
	}
	for _, tc := range []struct {
		name   string
		change func(*Message)
	}{
		{"ordinary_reply", func(m *Message) { m.GuestBotCallerUser = nil }},
		{"different_caller", func(m *Message) { m.GuestBotCallerUser.ID = 51 }},
		{"bot_caller", func(m *Message) { m.GuestBotCallerUser.IsBot = true }},
		{"human_sender", func(m *Message) { m.From.IsBot = false }},
		{"channel_sender", func(m *Message) { m.SenderChat = &Chat{ID: -200} }},
		{"channel_caller", func(m *Message) { m.Reply.SenderChat = &Chat{ID: -100} }},
		{"cross_chat", func(m *Message) { m.Reply.Chat.ID = -200 }},
		{"missing_reply", func(m *Message) { m.Reply = nil }},
		{"automatic_forward", func(m *Message) { m.IsAutomaticForward = true }},
		{"quoted_sync", func(m *Message) { m.Reply.IsAutomaticForward = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := invocationMessage(t)
			tc.change(m)
			if m.GuestInvocation() != nil {
				t.Fatal("untrusted invocation accepted")
			}
		})
	}
}

func TestInvocationFingerprintTracksEditsAndIgnoresNestedReply(t *testing.T) {
	m := invocationMessage(t).Reply
	want := m.InvocationFingerprint()
	m.Reply = &Message{ID: 9, Text: "background quote"}
	m.From.FirstName = "Renamed"
	if m.InvocationFingerprint() != want {
		t.Fatal("background or mutable display name changed target identity")
	}
	m.Text = "ordinary edited message"
	if m.InvocationFingerprint() == want {
		t.Fatal("edited message kept old invocation identity")
	}
	m = invocationMessage(t).Reply
	m.EditDate = 456
	if m.InvocationFingerprint() == want {
		t.Fatal("edit version not covered by invocation identity")
	}
}

func TestInvocationFingerprintUsesStableMentionedUserIdentity(t *testing.T) {
	m := invocationMessage(t).Reply
	m.Entities = []Entity{{Type: "text_mention", Offset: 0, Length: 3, User: &User{ID: 77, FirstName: "Old", LastName: "Name", Username: "old_name"}}}
	fingerprint := m.InvocationFingerprint()
	m.Entities[0].User.FirstName = "New"
	m.Entities[0].User.LastName = "Display"
	m.Entities[0].User.Username = "new_name"
	if m.InvocationFingerprint() != fingerprint {
		t.Fatal("mutable mentioned-user details changed invocation identity")
	}
	m.Entities[0].User.ID = 78
	if m.InvocationFingerprint() == fingerprint {
		t.Fatal("different mentioned user kept same invocation identity")
	}
}
