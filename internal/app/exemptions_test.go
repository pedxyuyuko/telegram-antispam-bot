package app

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func deliverRawMessage(t *testing.T, a *App, id int64, rawMessage string) {
	t.Helper()
	raw := json.RawMessage(fmt.Sprintf(`{"update_id":%d,"message":%s}`, id, rawMessage))
	if err := a.Store.Capture(context.Background(), []json.RawMessage{raw}); err != nil {
		t.Fatal(err)
	}
	if worked, err := a.ProcessInbox(context.Background()); err != nil || !worked {
		t.Fatalf("inbox: %v %v", worked, err)
	}
}

func TestGroupAdministratorsAreArchivedWithoutInference(t *testing.T) {
	for _, role := range []string{"administrator", "creator"} {
		t.Run(role, func(t *testing.T) {
			a, path := appFixture(t, role)
			modelFixture(t, a, path, nil)
			log := captureDebug(a)
			if err := a.Config.Set(nil, "debug", json.RawMessage(`true`)); err != nil {
				t.Fatal(err)
			}
			deliver(t, a, 1, message(50, "administrator discussion"))
			if scalar(t, path, `SELECT count(*) FROM revisions WHERE status='ignored' AND json_extract(snapshot,'$.skip_reason')='group_administrator'`) != 1 {
				t.Fatal("administrator message entered inference queue")
			}
			if worked, err := a.ProcessModel(context.Background(), time.Now()); err != nil || worked {
				t.Fatalf("administrator inferred: %v %v", worked, err)
			}
			if scalar(t, path, `SELECT count(*) FROM attempts`) != 0 {
				t.Fatal("administrator model attempt recorded")
			}
			rows := events(t, log, "message.archived")
			if len(rows) != 1 || rows[0]["skip_reason"] != "group_administrator" {
				t.Fatal("administrator exemption not explained in logs")
			}
			m := message(50, `/as_set queue_limit 7`)
			m.ID = 2
			deliver(t, a, 2, m)
			if a.Config.Snapshot().ForGroup(-100).QueueLimit != 7 || scalar(t, path, `SELECT count(*) FROM revisions WHERE status='command'`) != 1 {
				t.Fatal("administrator exemption disabled management commands")
			}
		})
	}
}

func TestChannelSyncAndAnonymousAdminMessagesAreOnlyArchived(t *testing.T) {
	for _, tc := range []struct{ name, raw, reason string }{
		{"channel_sync", `{"message_id":1,"chat":{"id":-100,"type":"supergroup"},"from":{"id":1087968824,"is_bot":true,"first_name":"channel"},"sender_chat":{"id":-200,"type":"channel"},"is_automatic_forward":true,"forward_origin":{"type":"channel","chat":{"id":-200,"type":"channel"},"message_id":11,"date":1},"audio":{"file_id":"fixture","duration":208}}`, "automatic_channel_forward"},
		{"anonymous_admin", `{"message_id":1,"chat":{"id":-100,"type":"supergroup"},"from":{"id":1087968824,"is_bot":true,"first_name":"anonymous"},"sender_chat":{"id":-100,"type":"supergroup"},"text":"anonymous administrator message"}`, "anonymous_administrator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, path := appFixture(t, "member")
			modelFixture(t, a, path, nil)
			deliverRawMessage(t, a, 1, tc.raw)
			if scalar(t, path, `SELECT count(*) FROM revisions WHERE status='ignored' AND json_extract(snapshot,'$.skip_reason')='`+tc.reason+`'`) != 1 {
				t.Fatal("exempt source entered inference queue")
			}
			if worked, err := a.ProcessModel(context.Background(), time.Now()); err != nil || worked {
				t.Fatalf("exempt source inferred: %v %v", worked, err)
			}
			if scalar(t, path, `SELECT count(*) FROM actions`) != 0 || scalar(t, path, `SELECT count(*) FROM attempts`) != 0 {
				t.Fatal("exempt source generated work")
			}
		})
	}
}

func TestMemberForwardingAndRepliesToSyncedPostsRemainEligible(t *testing.T) {
	for _, extra := range []string{
		`,"forward_origin":{"type":"channel","chat":{"id":-200,"type":"channel"},"message_id":11,"date":1}`,
		`,"reply_to_message":{"message_id":11,"chat":{"id":-100,"type":"supergroup"},"is_automatic_forward":true,"sender_chat":{"id":-200,"type":"channel"},"text":"synced post"}`,
	} {
		t.Run(extra, func(t *testing.T) {
			a, path := appFixture(t, "member")
			modelFixture(t, a, path, nil)
			deliverRawMessage(t, a, 1, `{"message_id":1,"chat":{"id":-100,"type":"supergroup"},"from":{"id":50,"first_name":"member"},"text":"member contribution"`+extra+`}`)
			runModel(t, a, time.Now())
			if scalar(t, path, `SELECT count(*) FROM revisions WHERE status='succeeded'`) != 1 {
				t.Fatal("ordinary member forwarding/reply was exempted")
			}
		})
	}
}
