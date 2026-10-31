package app

import (
	"context"
	"testing"
	"time"
)

func TestMalformedContextIsArchivedAsFailedWork(t *testing.T) {
	a, path := appFixture(t, "member")
	modelFixture(t, a, path, nil)
	deliverRawMessage(t, a, 1, `{"message_id":1,"chat":{"id":-100,"type":"supergroup"},"from":{"id":50,"first_name":"Member"},"text":"sample","reply_markup":{"inline_keyboard":42}}`)
	runModel(t, a, time.Now())
	if scalar(t, path, `SELECT count(*) FROM attempts WHERE error LIKE 'build model context:%' AND request=''`) != 1 {
		t.Fatal("context error was not retained before inference")
	}
	if scalar(t, path, `SELECT count(*) FROM revisions WHERE status='pending'`) != 1 {
		t.Fatal("failed context left work stuck as running")
	}
}

func TestFrozenLegacyContextIsReusedAfterUpgrade(t *testing.T) {
	a, path := appFixture(t, "member")
	modelFixture(t, a, path, nil)
	deliver(t, a, 1, message(50, "queued"))
	id := scalar(t, path, `SELECT id FROM revisions LIMIT 1`)
	legacy := []byte(`{"telegram_message":{"text":"frozen-original"},"sender_profile":{"bio":"frozen-profile"},"expanded_entities":[]}`)
	if err := a.Store.SetState(context.Background(), int64(id), legacy); err != nil {
		t.Fatal(err)
	}
	runModel(t, a, time.Now())
	if scalar(t, path, `SELECT count(*) FROM attempts WHERE json_extract(request,'$.state.telegram_message.text')='frozen-original' AND json_extract(request,'$.state.sender_profile.bio')='frozen-profile'`) != 1 {
		t.Fatal("historical inference context was rebuilt")
	}
}
