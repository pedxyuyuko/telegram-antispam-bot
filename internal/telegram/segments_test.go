package telegram

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSegmentedMessageRetainsTextOrderAndMentionProfiles(t *testing.T) {
	m := fingerprintMessage(t, `{"text":"🙂 @firstbot call @secondbot end","entities":[{"type":"mention","offset":3,"length":9},{"type":"mention","offset":18,"length":10}]}`)
	before := string(m.Bytes())
	refs := map[string]ReferenceProfile{"username:firstbot": {Username: "firstbot", Name: "First", Bio: "first bio"}, "username:secondbot": {Username: "secondbot", Name: "Second", Bio: "second bio"}}
	raw, err := m.SegmentedModelState(Profile{Bio: "sender bio"}, refs)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Sender  map[string]any `json:"sender"`
		Message []struct {
			Type, Text string
			Profile    *ReferenceProfile `json:"profile"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	var joined string
	var names []string
	for _, part := range state.Message {
		joined += part.Text
		if part.Type == "mention" {
			if part.Profile == nil {
				t.Fatal("mention profile missing")
			}
			names = append(names, part.Profile.Name)
		}
	}
	if joined != m.Text || strings.Join(names, ",") != "First,Second" || state.Sender["bio"] != "sender bio" || string(m.Bytes()) != before {
		t.Fatalf("segmentation lost source attribution: %s", raw)
	}
	var root map[string]any
	json.Unmarshal(raw, &root)
	if _, old := root["msg"]; old {
		t.Fatal("old message wrapper remains")
	}
}

func TestSegmentsKeepCaptionHiddenLinksButtonsAndMedia(t *testing.T) {
	m := fingerprintMessage(t, `{"caption":"打开频道","caption_entities":[{"type":"text_link","offset":0,"length":4,"url":"https://t.me/examplechannel/12"}],"photo":[{"file_unique_id":"photo-a"}],"reply_markup":{"inline_keyboard":[[{"text":"加入","url":"https://t.me/examplechannel"}],[{"text":"加入","url":"https://t.me/examplechannel"}]]},"guest_bot_caller_user":{"first_name":"Caller"}}`)
	refs := map[string]ReferenceProfile{"username:examplechannel": {Username: "examplechannel", Name: "Channel", Bio: "channel bio", Kind: "channel"}}
	raw, err := m.SegmentedModelState(Profile{}, refs)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{`"message":[`, `"type":"link"`, `"text":"打开频道"`, `"source":"caption"`, `"name":"Channel"`, `"count":2`, `"type":"media"`, `"caller":"Caller"`} {
		if !strings.Contains(string(raw), text) {
			t.Errorf("missing %s in %s", text, raw)
		}
	}
}

func TestTelegramReferencesNormalizePublicLinksAndPreserveUnsupportedInvites(t *testing.T) {
	for _, tc := range []struct {
		input, key, username string
		valid                bool
	}{
		{"@SomeChannel", "username:somechannel", "somechannel", true},
		{"https://t.me/SomeChannel/123?single", "username:somechannel", "somechannel", true},
		{"https://t.me/s/SomeChannel/123", "username:somechannel", "somechannel", true},
		{"t.me/SomeChannel", "username:somechannel", "somechannel", true},
		{"https://t.me/+Abc123", "invite:https://t.me/+Abc123", "", true},
		{"https://t.me.evil.test/SomeChannel", "", "", false},
		{"https://t.me@evil.test/SomeChannel", "", "", false},
		{"https://t.me/c/123/456", "", "", false},
		{"https://t.me/share/url?url=anything", "", "", false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			ref, ok := ParseReference(tc.input)
			if ok != tc.valid || ref.Key != tc.key || ref.Username != tc.username {
				t.Fatalf("reference=%+v ok=%v", ref, ok)
			}
		})
	}
}

func TestSegmentDiscoveryAvoidsEmailAndPreservesInvalidUTF16Entities(t *testing.T) {
	m := fingerprintMessage(t, `{"text":"mail@example.com 🙂 @adbot https://t.me/examplechannel.","entities":[{"type":"mention","offset":18,"length":1}]}`)
	refs := m.References(Profile{})
	if len(refs) != 2 || refs[0].Username != "adbot" || refs[1].Username != "examplechannel" {
		t.Fatalf("references=%+v", refs)
	}
	raw, err := m.SegmentedModelState(Profile{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Message []struct{ Text string } `json:"message"`
	}
	json.Unmarshal(raw, &state)
	var joined string
	for _, part := range state.Message {
		joined += part.Text
	}
	if joined != m.Text {
		t.Fatal("invalid UTF16 entity changed text")
	}
}

func TestReferenceProfilesExpandBiographyLinksOneLevel(t *testing.T) {
	m := fingerprintMessage(t, `{"text":"@adbot run"}`)
	profiles := map[string]ReferenceProfile{
		"username:adbot":   {Username: "adbot", Name: "Bot", Bio: "join https://t.me/channel"},
		"username:channel": {Username: "channel", Name: "Channel", Bio: "https://t.me/another"},
		"username:another": {Username: "another", Name: "TooDeep"},
	}
	raw, err := m.SegmentedModelState(Profile{}, profiles)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"name":"Channel"`) || strings.Contains(string(raw), `"name":"TooDeep"`) {
		t.Fatalf("wrong biography expansion depth: %s", raw)
	}
}

func TestSegmentedButtonsWithOnlyCallbackDataKeepTheirText(t *testing.T) {
	m := fingerprintMessage(t, `{"reply_markup":{"inline_keyboard":[[{"text":"操作","callback_data":"opaque"}]]}}`)
	raw, err := m.SegmentedModelState(Profile{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"text":"操作"`) {
		t.Fatal("button label lost")
	}
}

func TestSegmentedFingerprintRequiresAndVersionsQueryableProfiles(t *testing.T) {
	m := fingerprintMessage(t, `{"text":"@examplechannel run"}`)
	if f, err := m.SegmentedContentFingerprint(nil); err != nil || f.Hash != "" {
		t.Fatal("reference-free admission key invented")
	}
	state, err := m.SegmentedModelState(Profile{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := m.SegmentedContentFingerprint(state); err != nil || f.Hash != "" {
		t.Fatal("failed profile entered cache")
	}
	var previous string
	for _, bio := range []string{"first", "second"} {
		state, err = m.SegmentedModelState(Profile{}, map[string]ReferenceProfile{"username:examplechannel": {Username: "examplechannel", Name: "Channel", Bio: bio}})
		if err != nil {
			t.Fatal(err)
		}
		f, err := m.SegmentedContentFingerprint(state)
		if err != nil || f.Hash == "" || f.Hash == previous {
			t.Fatal("profile snapshot missing from cache key")
		}
		previous = f.Hash
	}
}

func TestSegmentedFingerprintKeepsPureAdCacheIndependentOfCaller(t *testing.T) {
	a := fingerprintMessage(t, `{"text":"ad","from":{"id":200,"is_bot":true},"guest_bot_caller_user":{"first_name":"one"},"reply_markup":{"inline_keyboard":[[{"text":"join","url":"https://t.me/+invite"}]]}}`)
	b := fingerprintMessage(t, `{"text":"ad","from":{"id":201,"is_bot":true},"guest_bot_caller_user":{"first_name":"two"},"reply_markup":{"inline_keyboard":[[{"text":"join","url":"https://t.me/+invite"}]]}}`)
	x, err := a.SegmentedContentFingerprint(nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err := b.SegmentedModelState(Profile{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	y, err := b.SegmentedContentFingerprint(state)
	if err != nil {
		t.Fatal(err)
	}
	if x.Hash == "" || x.Hash != y.Hash {
		t.Fatal("caller or unsupported invitation disabled pure ad reuse")
	}
	old, err := a.ContentFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if old.Hash == x.Hash {
		t.Fatal("context format failed to isolate old cache")
	}
}

func TestReferenceDiscoveryValidatesTheWholeURLHost(t *testing.T) {
	for _, text := range []string{"https://evil-t.me/alice", "https://evil-telegram.me/alice", "https://evil.test/?next=https://t.me/alice", "evil-t.me/alice", "https://中t.me/alice"} {
		if refs := ReferencesInText(text); len(refs) != 0 {
			t.Fatalf("foreign URL produced Telegram reference: %s %+v", text, refs)
		}
	}
	if refs := ReferencesInText("进群 https://t.me/alice"); len(refs) != 1 || refs[0].Username != "alice" {
		t.Fatal("valid Telegram link lost")
	}
}
