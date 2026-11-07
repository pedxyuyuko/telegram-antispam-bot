package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/vision"
)

func imageFixture(t *testing.T, c color.Color) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 320, 280))
	for y := 0; y < 280; y++ {
		for x := 0; x < 320; x++ {
			im.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestImageSourcesChooseCurrentMediumPhotoAndStickerPreview(t *testing.T) {
	for _, tc := range []struct {
		raw, id, kind string
		preview       bool
	}{
		{`{"photo":[{"file_id":"large","file_unique_id":"l","width":1280,"height":1280},{"file_id":"tiny","file_unique_id":"t","width":320,"height":320},{"file_id":"medium","file_unique_id":"m","width":800,"height":800}],"reply_to_message":{"photo":[{"file_id":"reply","width":800,"height":800}]}}`, "medium", "photo", false},
		{`{"sticker":{"file_id":"full","file_unique_id":"s","width":512,"height":512,"thumbnail":{"file_id":"thumb","file_unique_id":"st","width":320,"height":320}}}`, "full", "sticker", false},
		{`{"sticker":{"file_id":"video","is_video":true,"thumbnail":{"file_id":"thumb","file_unique_id":"st","width":320,"height":320}}}`, "thumb", "sticker", true},
		{`{"sticker":{"file_id":"animated","is_animated":true,"thumb":{"file_id":"legacy-thumb","width":320,"height":320}}}`, "legacy-thumb", "sticker", true},
		{`{"sticker":{"file_id":"video","is_video":true}}`, "", "sticker", true},
	} {
		m := fingerprintMessage(t, tc.raw)
		sources := m.ImageSources()
		if len(sources) != 1 || sources[0].FileID != tc.id || sources[0].Kind != tc.kind || sources[0].Preview != tc.preview {
			t.Fatalf("sources=%+v", sources)
		}
	}
	m := fingerprintMessage(t, `{"text":"reply only","reply_to_message":{"photo":[{"file_id":"reply","width":800,"height":800}]}}`)
	if len(m.ImageSources()) != 0 {
		t.Fatal("reply image was fetched")
	}
}

func imageClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	cfg := config.Defaults().Telegram
	cfg.APIBase = s.URL
	c, err := New(cfg, "123:secret", s.Client(), 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestImageLookupUsesBotFileAPIAndRetainsSafeAudit(t *testing.T) {
	raw := imageFixture(t, color.RGBA{R: 255, A: 255})
	var downloads atomic.Int32
	c := imageClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			r.ParseMultipartForm(1 << 20)
			if r.FormValue("file_id") != "f" {
				t.Error("wrong file request")
			}
			w.Write([]byte(`{"ok":true,"result":{"file_id":"f","file_unique_id":"u","file_path":"photos/f.png"}}`))
			return
		}
		if r.URL.Path != "/file/bot123:secret/photos/f.png" {
			t.Error("wrong file download path")
		}
		downloads.Add(1)
		w.Write(raw)
	})
	lookup := c.LookupImage(context.Background(), ImageSource{Kind: "photo", FileID: "f", FileUniqueID: "u"})
	if lookup.Status != "available" || lookup.Width != 320 || lookup.Height != 280 || lookup.SHA256 == "" || !bytes.Equal(lookup.Downloaded, raw) || lookup.Asset.Format != "png" || downloads.Load() != 1 {
		t.Fatalf("lookup=%+v", lookup)
	}
	audit, err := json.Marshal(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(audit, []byte("123:secret")) {
		t.Fatal("file URL credential leaked into audit")
	}
}

func TestImageLookupRejectsUnsafePathsAndIdentityMismatch(t *testing.T) {
	for _, tc := range []struct{ path, unique string }{{"../other", "u"}, {"/etc/passwd", "u"}, {"https://example.test/file", "u"}, {"photos/%2e%2e/secret", "u"}, {"photos/x?token=x", "u"}, {"photos\\x", "u"}, {"photos/f.png", "other"}} {
		var downloads atomic.Int32
		c := imageClient(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/getFile") {
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_id": "f", "file_unique_id": tc.unique, "file_path": tc.path}})
			} else {
				downloads.Add(1)
			}
		})
		got := c.LookupImage(context.Background(), ImageSource{Kind: "photo", FileID: "f", FileUniqueID: "u"})
		if got.Status == "available" || downloads.Load() != 0 {
			t.Fatalf("unsafe lookup=%+v downloads=%d", got, downloads.Load())
		}
	}
}

func TestImageLookupDoesNotFollowRedirectsOrReadOversizedFiles(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer other.Close()
	for _, redirect := range []bool{true, false} {
		c := imageClient(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/getFile") {
				w.Write([]byte(`{"ok":true,"result":{"file_id":"f","file_unique_id":"u","file_path":"photos/f"}}`))
				return
			}
			if redirect {
				http.Redirect(w, r, other.URL, http.StatusFound)
			} else {
				w.Header().Set("Content-Length", "4194305")
				w.WriteHeader(200)
			}
		})
		got := c.LookupImage(context.Background(), ImageSource{Kind: "photo", FileID: "f", FileUniqueID: "u"})
		if got.Status == "available" || len(got.Downloaded) > 0 || leaked.Load() != 0 {
			t.Fatal("redirect or oversized download escaped limits")
		}
	}
}

func TestCancelledImageBudgetKeepsTextAndAvoidsFileRequests(t *testing.T) {
	var calls atomic.Int32
	c := imageClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := fingerprintMessage(t, `{"text":"ad","photo":[{"file_id":"f","file_unique_id":"u","width":800,"height":800}]}`)
	lookups := c.MessageImages(ctx, m)
	if len(lookups) != 1 || lookups[0].Status != "limit" || calls.Load() != 0 {
		t.Fatal("expired budget dispatched file requests")
	}
	base, err := m.SegmentedModelState(Profile{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, assets, err := AttachImages(base, lookups)
	if err != nil || len(assets) != 0 || !bytes.Contains(state, []byte(`"text":"ad"`)) {
		t.Fatal("image failure lost text")
	}
}

func TestVisualFingerprintBindsBytesAndIgnoresRandomAssetReferences(t *testing.T) {
	m := fingerprintMessage(t, `{"text":"ad","photo":[{"file_id":"f","file_unique_id":"u","width":320,"height":280}],"reply_to_message":{"photo":[{"file_id":"reply","file_unique_id":"r"}]}}`)
	before := string(m.Bytes())
	base, err := m.SegmentedModelState(Profile{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := m.VisualContentFingerprint(nil, nil); err != nil || f.Hash != "" {
		t.Fatal("image admission bypassed actual bytes")
	}
	if f, err := m.VisualContentFingerprint(base, nil); err != nil || f.Hash != "" {
		t.Fatal("missing image published a content key")
	}
	var previous, firstRef string
	for i, c := range []color.Color{color.RGBA{R: 255, A: 255}, color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255}} {
		normalized, err := vision.Normalize(imageFixture(t, c))
		if err != nil {
			t.Fatal(err)
		}
		lookups := []ImageLookup{{Source: ImageSource{Kind: "photo"}, Status: "available", SHA256: normalized.SHA256, Asset: normalized.Asset()}}
		state, assets, err := AttachImages(base, lookups)
		if err != nil {
			t.Fatal(err)
		}
		if len(assets) != 1 || lookups[0].Reference == "" {
			t.Fatal("asset reference missing")
		}
		if i == 1 && lookups[0].Reference == firstRef {
			t.Fatal("new message reused an image reference")
		}
		if i == 0 {
			firstRef = lookups[0].Reference
		}
		f, err := m.VisualContentFingerprint(state, assets)
		if err != nil || f.Hash == "" {
			t.Fatalf("fingerprint=%+v %v", f, err)
		}
		if i == 1 && f.Hash != previous {
			t.Fatal("random reference changed content fingerprint")
		}
		if i == 2 && f.Hash == previous {
			t.Fatal("different image bytes reused fingerprint")
		}
		previous = f.Hash
		plain, err := WithoutImages(state, assets)
		if err != nil || bytes.Contains(plain, []byte(lookups[0].Reference)) || !bytes.Contains(plain, []byte(`"text":"ad"`)) {
			t.Fatal("fallback lost text or retained dangling asset")
		}
		var body map[string]json.RawMessage
		json.Unmarshal(state, &body)
		if bytes.Contains(body["reply"], []byte(`"image"`)) {
			t.Fatal("image attached to reply")
		}
	}
	if string(m.Bytes()) != before {
		t.Fatal("raw message altered")
	}
}
