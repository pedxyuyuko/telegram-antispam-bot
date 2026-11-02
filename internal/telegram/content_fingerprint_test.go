package telegram

import (
	"encoding/json"
	"testing"
)

func fingerprintMessage(t *testing.T, raw string) *Message {
	t.Helper()
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestContentFingerprintSharesAdsAcrossBotsChatsAndCallers(t *testing.T) {
	a := fingerprintMessage(t, `{"message_id":1,"date":10,"chat":{"id":-100,"type":"supergroup"},"from":{"id":200,"is_bot":true,"first_name":"one"},"text":"ad","guest_bot_caller_user":{"id":50,"first_name":"caller"},"reply_to_message":{"message_id":9,"text":"@one"},"reply_markup":{"inline_keyboard":[[{"text":"buy","url":"https://example.test/a"}]]}}`)
	b := fingerprintMessage(t, `{"text":"ad","from":{"first_name":"two","username":"two_bot","id":201,"is_bot":true},"chat":{"type":"supergroup","id":-200},"message_id":22,"date":99,"guest_bot_caller_user":{"id":51,"first_name":"other"},"reply_markup":{"inline_keyboard":[[{"url":"https://example.test/a","text":"buy"}]]}}`)
	x, err := a.ContentFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	y, err := b.ContentFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Hash) != 64 || x.Hash != y.Hash || string(x.Material) != string(y.Material) {
		t.Fatalf("same ad changed fingerprint: %+v %+v", x, y)
	}
}

func TestContentFingerprintSeparatesMeaningfulContent(t *testing.T) {
	base := `{"from":{"id":200,"is_bot":true},"text":"ad","reply_markup":{"inline_keyboard":[[{"text":"buy","url":"https://example.test/a"}]]}}`
	first, err := fingerprintMessage(t, base).ContentFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"from":{"id":200,"is_bot":true},"text":"warning: ad","reply_markup":{"inline_keyboard":[[{"text":"buy","url":"https://example.test/a"}]]}}`,
		`{"from":{"id":200,"is_bot":true},"text":"ad","reply_markup":{"inline_keyboard":[[{"text":"buy","url":"https://example.test/b"}]]}}`,
		`{"from":{"id":200,"is_bot":true},"text":"ad","entities":[{"type":"blockquote","offset":0,"length":2}],"reply_markup":{"inline_keyboard":[[{"text":"buy","url":"https://example.test/a"}]]}}`,
		`{"from":{"id":50,"is_bot":false},"text":"ad","reply_markup":{"inline_keyboard":[[{"text":"buy","url":"https://example.test/a"}]]}}`,
	} {
		next, err := fingerprintMessage(t, raw).ContentFingerprint()
		if err != nil {
			t.Fatal(err)
		}
		if first.Hash == next.Hash {
			t.Fatal("different content or sender kind shares fingerprint")
		}
	}
}

func TestContentFingerprintPreservesMediaIdentityAndSkipsOpaqueContent(t *testing.T) {
	var previous string
	for _, id := range []string{"image-a", "image-b"} {
		f, err := fingerprintMessage(t, `{"caption":"ad","photo":[{"file_id":"ephemeral","file_unique_id":"`+id+`"}]}`).ContentFingerprint()
		if err != nil {
			t.Fatal(err)
		}
		if f.Hash == "" || f.Hash == previous {
			t.Fatal("media identity lost")
		}
		previous = f.Hash
	}
	for _, raw := range []string{`{"photo":[{"file_id":"opaque"}]}`, `{"text":""}`, `{"poll":{"question":"different opaque content"}}`} {
		f, err := fingerprintMessage(t, raw).ContentFingerprint()
		if err != nil {
			t.Fatal(err)
		}
		if f.Hash != "" {
			t.Fatal("opaque payload became cacheable")
		}
	}
}
