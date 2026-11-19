package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
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

func TestImageAuditRedactsCredentialsEchoedInReturnedFilePaths(t *testing.T) {
	for _, p := range []string{"/local/bot123:secret/photos/f.png", "https://example.test/bot123%3Asecret/photos/f.png"} {
		c := imageClient(t, func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_id": "f", "file_unique_id": "u", "file_path": p}})
		})
		lookup := c.LookupImage(context.Background(), ImageSource{Kind: "photo", FileID: "f", FileUniqueID: "u"})
		raw, err := json.Marshal(lookup)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("123:secret")) || bytes.Contains(raw, []byte("123%3Asecret")) {
			t.Fatal("upstream file path copied a credential into image audit")
		}
		if lookup.Status == "available" {
			t.Fatal("absolute file path accepted")
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

func TestImageEnrichmentPreservesPresentedContextOrder(t *testing.T) {
	before := json.RawMessage(`{"counter":9007199254740993,"ratio":1.00,"sender":{"name":"Member","bio":null},"reply":{"sender":"Quoted","text":"context"},"message":[{"type":"text","text":"caption"},{"type":"media","media":[{"type":"photo"}]}]}`)
	im, err := vision.Normalize(imageFixture(t, color.Black))
	if err != nil {
		t.Fatal(err)
	}
	for _, preview := range []bool{false, true} {
		lookups := []ImageLookup{{Source: ImageSource{Kind: "photo", Preview: preview}, Status: "available", Asset: im.Asset(), SHA256: im.SHA256}}
		state, assets, err := AttachImages(before, lookups)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Index(state, []byte(`"sender"`)) > bytes.Index(state, []byte(`"message"`)) {
			t.Fatal("image enrichment moved current content ahead of sender/reply context")
		}
		plain, err := WithoutImages(state, assets)
		if err != nil || !bytes.Equal(before, plain) {
			t.Fatalf("text fallback changed existing presentation: %s %v", plain, err)
		}
	}
}

func twoFrameGIF(t *testing.T) []byte {
	t.Helper()
	pal := []color.Color{color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255}}
	frame := func(i uint8) *image.Paletted {
		f := image.NewPaletted(image.Rect(0, 0, 320, 280), pal)
		for p := range f.Pix {
			f.Pix[p] = i
		}
		return f
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, &gif.GIF{Image: []*image.Paletted{frame(0), frame(1)}, Delay: []int{10, 10}}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestImageSourcesUseAnimationThumbnailsAndGIFDocumentFrames(t *testing.T) {
	for _, tc := range []struct {
		raw, kind, id   string
		frames, preview bool
	}{
		// The MP4 itself is never fetched: only Telegram's own still preview.
		{`{"animation":{"file_id":"a","file_unique_id":"ua","width":320,"height":280,"mime_type":"video/mp4","file_size":1000,"thumbnail":{"file_id":"th","file_unique_id":"ut","width":320,"height":280}},"document":{"file_id":"a","file_unique_id":"ua","mime_type":"video/mp4"}}`, "animation", "th", false, true},
		{`{"animation":{"file_id":"a","file_unique_id":"ua","mime_type":"video/mp4","thumb":{"file_id":"old","file_unique_id":"uo","width":90,"height":90}}}`, "animation", "old", false, true},
		{`{"animation":{"file_id":"a","file_unique_id":"ua","mime_type":"video/mp4"}}`, "animation", "", false, true},
		{`{"document":{"file_id":"g","file_unique_id":"ug","mime_type":"image/gif","file_size":1000}}`, "document", "g", true, false},
		{`{"document":{"file_id":"big","file_unique_id":"ub","mime_type":"image/gif","file_size":5000000,"thumbnail":{"file_id":"th","file_unique_id":"ut","width":320,"height":180}}}`, "document", "th", false, true},
	} {
		sources := fingerprintMessage(t, tc.raw).ImageSources()
		if len(sources) != 1 || sources[0].Kind != tc.kind || sources[0].FileID != tc.id || sources[0].Frames != tc.frames || sources[0].Preview != tc.preview {
			t.Fatalf("%s: sources=%+v", tc.raw, sources)
		}
	}
	if s := fingerprintMessage(t, `{"document":{"file_id":"d","mime_type":"application/pdf"}}`).ImageSources(); len(s) != 0 {
		t.Fatalf("non-image document became image source: %+v", s)
	}
}

func TestGIFDocumentLookupDownloadsOnceAndReturnsEndFrames(t *testing.T) {
	raw := twoFrameGIF(t)
	var downloads atomic.Int32
	c := imageClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			w.Write([]byte(`{"ok":true,"result":{"file_id":"g","file_unique_id":"ug","file_path":"documents/g.gif"}}`))
			return
		}
		downloads.Add(1)
		w.Write(raw)
	})
	lookup := c.LookupImage(context.Background(), ImageSource{Kind: "document", Frames: true, FileID: "g", FileUniqueID: "ug"})
	if lookup.Status != "available" || downloads.Load() != 1 || !bytes.Equal(lookup.Downloaded, raw) || len(lookup.Frames) != 2 {
		t.Fatalf("lookup=%+v", lookup)
	}
	if lookup.Frames[0].Position != "first" || lookup.Frames[1].Position != "last" || lookup.Frames[0].SHA256 == lookup.Frames[1].SHA256 || lookup.Frames[1].Asset.Format != "png" {
		t.Fatalf("frames=%+v", lookup.Frames)
	}
}

func TestGIFFramesAttachFingerprintAndFallBackCleanly(t *testing.T) {
	m := fingerprintMessage(t, `{"caption":"ad","document":{"file_id":"g","file_unique_id":"ug","mime_type":"image/gif","file_size":1000}}`)
	base, err := m.SegmentedModelState(Profile{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	frame := func(c color.Color) ImageFrame {
		im, err := vision.Normalize(imageFixture(t, c))
		if err != nil {
			t.Fatal(err)
		}
		return ImageFrame{SHA256: im.SHA256, Width: im.Width, Height: im.Height, Asset: im.Asset()}
	}
	red, blue := frame(color.RGBA{R: 255, A: 255}), frame(color.RGBA{B: 255, A: 255})
	var previous string
	for i, ends := range [][2]ImageFrame{{red, blue}, {red, blue}, {red, red}} {
		first, last := ends[0], ends[1]
		first.Position, last.Position = "first", "last"
		lookups := []ImageLookup{{Source: ImageSource{Kind: "document", Frames: true}, Status: "available", Frames: []ImageFrame{first, last}}}
		state, assets, err := AttachImages(base, lookups)
		if err != nil {
			t.Fatal(err)
		}
		if len(assets) != 2 || lookups[0].Frames[0].Reference == "" || lookups[0].Frames[1].Reference == "" {
			t.Fatalf("frame references missing: %s", state)
		}
		_, media, _ := currentMedia(state)
		frames, _ := media[0]["frames"].([]any)
		if len(media) != 1 || len(frames) != 2 || frames[0].(map[string]any)["position"] != "first" || frames[1].(map[string]any)["image"] != lookups[0].Frames[1].Reference || media[0]["image"] != nil {
			t.Fatalf("frames attached to wrong item: %s", state)
		}
		f, err := m.VisualContentFingerprint(state, assets)
		if err != nil || f.Hash == "" {
			t.Fatalf("fingerprint=%+v %v", f, err)
		}
		if i == 1 && f.Hash != previous {
			t.Fatal("random frame references changed the content key")
		}
		if i == 2 && f.Hash == previous {
			t.Fatal("different last frame reused the content key")
		}
		previous = f.Hash
		partial := vision.Assets{lookups[0].Frames[0].Reference: assets[lookups[0].Frames[0].Reference]}
		if f, _ := m.VisualContentFingerprint(state, partial); f.Hash != "" {
			t.Fatal("missing frame published a content key")
		}
		plain, err := WithoutImages(state, assets)
		if err != nil || !bytes.Equal(plain, base) {
			t.Fatalf("text fallback kept frame material: %s %v", plain, err)
		}
	}
}
