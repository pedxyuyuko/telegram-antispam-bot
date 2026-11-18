package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/telegram"
	"telegram-antispam-bot/internal/vision"
)

type imageRemote struct {
	enriched                          atomic.Bool
	files, downloads, models          atomic.Int32
	fileFailure, offline              atomic.Bool
	rejectCode                        atomic.Int32
	rejectField, imageBytes, lastFile atomic.Value
}

func appImageBytes(t *testing.T, c color.Color) []byte {
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

func imageApp(t *testing.T) (*App, string, *imageRemote) {
	t.Helper()
	a, path := appFixture(t, "member")
	r := &imageRemote{}
	r.rejectField.Store("state")
	r.lastFile.Store("")
	r.imageBytes.Store(appImageBytes(t, color.RGBA{R: 255, A: 255}))
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/systemone" {
			r.models.Add(1)
			var body struct {
				Model     string
				State     json.RawMessage
				Questions map[string]config.Question
				Assets    vision.Assets
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if r.offline.Load() {
				http.Error(w, "offline", 503)
				return
			}
			if len(body.Assets) > 0 {
				if req.Header.Get("TypeSafe-Danger-Zone-Images") != "true" {
					t.Error("image compatibility header missing")
				}
				for ref, asset := range body.Assets {
					data, err := base64.StdEncoding.Strict().DecodeString(asset.DataBase64)
					if err != nil {
						t.Error(err)
					}
					cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
					if err != nil || format != asset.Format || cfg.Width < 280 || cfg.Height < 280 || cfg.Width > 1568 || cfg.Height > 1568 || !bytes.Contains(body.State, []byte(ref)) {
						t.Error("invalid or unused model image asset")
					}
				}
				if code := r.rejectCode.Load(); code != 0 {
					w.WriteHeader(int(code))
					if code == 422 {
						fmt.Fprintf(w, `{"detail":[{"loc":["body",%q],"msg":"image unavailable","type":"value_error"}]}`, r.rejectField.Load().(string))
					} else {
						fmt.Fprint(w, `{"detail":"invalid image"}`)
					}
					return
				}
			}
			answers := map[string]any{}
			for id := range body.Questions {
				answers[id] = map[string]any{"type": "noul", "noul": .99}
			}
			json.NewEncoder(w).Encode(map[string]any{"model": body.Model, "answers": answers})
			return
		}
		if strings.HasPrefix(req.URL.Path, "/file/") {
			r.downloads.Add(1)
			w.Write(r.imageBytes.Load().([]byte))
			return
		}
		req.ParseMultipartForm(1 << 20)
		if req.MultipartForm != nil {
			defer req.MultipartForm.RemoveAll()
		}
		switch {
		case strings.HasSuffix(req.URL.Path, "/getChatMember"):
			id, _ := strconv.ParseInt(req.FormValue("user_id"), 10, 64)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"status": "member", "user": map[string]any{"id": id, "first_name": "Member"}}})
		case strings.HasSuffix(req.URL.Path, "/getChat"):
			id, _ := strconv.ParseInt(req.FormValue("chat_id"), 10, 64)
			if r.enriched.Load() {
				if id == -88 {
					fmt.Fprint(w, `{"ok":true,"result":{"id":-88,"type":"channel","title":"Showcase","description":"Channel description","photo":{"small_file_id":"channel-pic"}}}`)
					return
				}
				fmt.Fprint(w, `{"ok":true,"result":{"id":50,"type":"private","bio":"profile bio","emoji_status_custom_emoji_id":"777","photo":{"small_file_id":"avatar"},"personal_chat":{"id":-88,"type":"channel","title":"Showcase","username":"showcase"}}}`)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"id": id, "type": "private", "bio": "sender biography"}})
		case strings.HasSuffix(req.URL.Path, "/getUserPersonalChatMessages"):
			fmt.Fprint(w, `{"ok":true,"result":[{"message_id":7,"date":12345,"chat":{"id":-88,"type":"channel"},"caption":"Latest caption","photo":[{"file_id":"post-photo","width":320,"height":320}]}]}`)
		case strings.HasSuffix(req.URL.Path, "/getChatMemberCount"):
			fmt.Fprint(w, `{"ok":true,"result":24}`)
		case strings.HasSuffix(req.URL.Path, "/getCustomEmojiStickers"):
			fmt.Fprint(w, `{"ok":true,"result":[{"type":"custom_emoji","custom_emoji_id":"777","emoji":"✅","set_name":"Pack","is_video":true,"file_id":"video","thumbnail":{"file_id":"badge-preview"}},{"type":"custom_emoji","custom_emoji_id":"888","emoji":"🦷","set_name":"Pack","file_id":"tooth"}]}`)
		case strings.HasSuffix(req.URL.Path, "/getStickerSet"):
			fmt.Fprint(w, `{"ok":true,"result":{"name":"Pack","title":"Pack Title","stickers":[]}}`)
		case strings.HasSuffix(req.URL.Path, "/getFile"):
			r.files.Add(1)
			r.lastFile.Store(req.FormValue("file_id"))
			if r.fileFailure.Load() {
				w.Write([]byte(`{"ok":false,"error_code":400,"description":"file unavailable"}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_id": req.FormValue("file_id"), "file_unique_id": "u", "file_path": "photos/file.png"}})
		default:
			t.Errorf("unexpected operation: %s", req.URL.Path)
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(s.Close)
	c := a.Config.Snapshot()
	c.Model.BaseURL = s.URL
	c.Model.RetrySeconds = 1
	c.Base.Mode = "record"
	c.QuestionGroups = map[string]map[string]config.Question{"content": {"ad": {Type: "noul", Instructions: "Current text and image advertisement?"}}}
	c.Base.Questions = []string{"content.ad"}
	c.Base.Scoring = nil
	c.Base.Condition = "p.content.ad >= 0.9"
	c.Base.Rules = map[string]config.Rule{"delete": {Enabled: true, Count: 1, Action: "delete"}}
	saveScoringConfig(t, a, path, c)
	cfg := c.Telegram
	cfg.APIBase = s.URL
	tg, err := telegram.New(cfg, "123:offline", s.Client(), 0, a.Log)
	if err != nil {
		t.Fatal(err)
	}
	tg.Me = a.Telegram.Me
	a.Telegram = tg
	return a, path, r
}

func TestSenderShowcaseAndEmojiReachNativeModel(t *testing.T) {
	a, path, r := imageApp(t)
	r.enriched.Store(true)
	r.rejectCode.Store(422)
	deliverRawMessage(t, a, 1, `{"message_id":1,"chat":{"id":-100,"type":"supergroup"},"from":{"id":50,"first_name":"Member","is_premium":true},"text":"A🦷","entities":[{"type":"custom_emoji","offset":1,"length":2,"custom_emoji_id":"888"}],"sticker":{"type":"regular","emoji":"♥","set_name":"Pack","file_id":"sticker","file_unique_id":"u"}}`)
	runModel(t, a, time.Now())
	const metadata = `json_extract(request,'$.state.sender.personal_channel.latest_post.caption')='Latest caption' AND json_extract(request,'$.state.sender.personal_channel.subscribers')=24 AND json_extract(request,'$.state.sender.emoji_status.set_title')='Pack Title' AND json_extract(request,'$.state.sender.id')=50 AND json_extract(request,'$.state.sender.is_premium')=1 AND request LIKE '%"type":"custom_emoji"%' AND request LIKE '%"custom_emoji_id":"888"%' AND json_extract(request,'$.state.message[1].media[0].set_title')='Pack Title'`
	if scalar(t, path, `SELECT count(*) FROM attempts WHERE `+metadata+` AND json_type(request,'$.state.sender.emoji_status.image')='text' AND json_extract(request,'$.state.sender.emoji_status.image_source')='thumbnail' AND json_type(request,'$.state.sender.personal_channel.latest_post.image')='text' AND (SELECT count(*) FROM json_each(request,'$.assets'))=6`) != 1 {
		t.Fatal("missing scoped profile/post/sticker metadata or pixels in visual request")
	}
	if scalar(t, path, `SELECT count(*) FROM attempts WHERE `+metadata+` AND json_type(request,'$.assets') IS NULL AND request NOT LIKE '%"image":%'`) != 1 {
		t.Fatal("fallback must retain metadata and remove all image references")
	}
}

func visualMessageRaw(id int, kind string) string {
	media := `"photo":[{"file_id":"photo-small","file_unique_id":"small","width":320,"height":280},{"file_id":"photo-medium","file_unique_id":"u","width":800,"height":700},{"file_id":"photo-large","file_unique_id":"large","width":1600,"height":1400}]`
	if kind == "sticker" {
		media = `"sticker":{"file_id":"static-sticker","file_unique_id":"u","width":512,"height":512,"emoji":"👍"}`
	}
	if kind == "video_sticker" {
		media = `"sticker":{"file_id":"full-video","file_unique_id":"video","width":512,"height":512,"emoji":"👍","is_video":true,"thumbnail":{"file_id":"sticker-thumb","file_unique_id":"u","width":320,"height":320}}`
	}
	return fmt.Sprintf(`{"message_id":%d,"chat":{"id":-100,"type":"supergroup"},"from":{"id":50,"first_name":"Member"},"text":"same caption",%s,"reply_to_message":{"message_id":99,"chat":{"id":-100,"type":"supergroup"},"from":{"id":51,"first_name":"Quoted"},"photo":[{"file_id":"reply-photo","file_unique_id":"reply","width":800,"height":800}]},"unknown":"preserved"}`, id, media)
}

func TestVisualMessagesSendPhotoAndStickerPixelsWithCurrentScope(t *testing.T) {
	for _, tc := range []struct{ kind, file string }{{"photo", "photo-medium"}, {"sticker", "static-sticker"}, {"video_sticker", "sticker-thumb"}} {
		t.Run(tc.kind, func(t *testing.T) {
			a, path, r := imageApp(t)
			if tc.kind == "sticker" {
				webp, _ := base64.StdEncoding.DecodeString("UklGRigAAABXRUJQVlA4TBsAAAAvP8FFAAdQj0oXpf8BAUnS//9kRP8z/vN/4AEA")
				r.imageBytes.Store(webp)
			}
			deliverRawMessage(t, a, 1, visualMessageRaw(1, tc.kind))
			runModel(t, a, time.Now())
			if r.files.Load() != 1 || r.downloads.Load() != 1 || r.models.Load() != 1 || r.lastFile.Load().(string) != tc.file {
				t.Fatalf("file=%v downloads=%d models=%d", r.lastFile.Load(), r.downloads.Load(), r.models.Load())
			}
			if scalar(t, path, `SELECT count(*) FROM attempts WHERE json_type(request,'$.assets')='object' AND json_extract(request,'$.state.message[0].text')='same caption' AND json_type(request,'$.state.reply.media[0].image') IS NULL`) != 1 {
				t.Fatal("asset or image ownership missing in actual request")
			}
			if scalar(t, path, `SELECT count(*) FROM revisions WHERE json_extract(snapshot,'$.context_version')=4 AND json_extract(raw,'$.unknown')='preserved'`) != 1 || scalar(t, path, `SELECT count(*) FROM revision_images WHERE json_extract(lookups,'$[0].status')='available'`) != 1 {
				t.Fatal("visual input or download evidence not retained")
			}
		})
	}
}

func TestImageFailureFallsBackWithoutLosingTextOrPublishingVisualCache(t *testing.T) {
	for _, kind := range []string{"download", "400", "413", "422"} {
		t.Run(kind, func(t *testing.T) {
			a, path, r := imageApp(t)
			if kind == "download" {
				r.fileFailure.Store(true)
			} else {
				code, _ := strconv.Atoi(kind)
				r.rejectCode.Store(int32(code))
			}
			deliverRawMessage(t, a, 1, visualMessageRaw(1, "photo"))
			runModel(t, a, time.Now())
			want := int32(2)
			if kind == "download" {
				want = 1
			}
			if r.models.Load() != want {
				t.Fatalf("model calls=%d want %d", r.models.Load(), want)
			}
			if scalar(t, path, `SELECT count(*) FROM attempts WHERE error='' AND json_type(request,'$.assets') IS NULL AND json_extract(request,'$.state.message[0].text')='same caption'`) != 1 {
				t.Fatal("plain-text fallback request missing")
			}
			if scalar(t, path, `SELECT count(*) FROM content_cache`) != 0 || scalar(t, path, `SELECT count(*) FROM meta WHERE key='model_retry_at'`) != 0 {
				t.Fatal("degraded image became cached evidence or paused queue")
			}
			if kind != "download" && scalar(t, path, `SELECT count(*) FROM attempts WHERE error<>'' AND json_type(request,'$.assets')='object' AND response<>''`) != 1 {
				t.Fatal("rejected image request was discarded")
			}
		})
	}
}

func TestImageOutageRetriesSameFrozenBytesAndReferences(t *testing.T) {
	a, path, r := imageApp(t)
	r.offline.Store(true)
	deliverRawMessage(t, a, 1, visualMessageRaw(1, "photo"))
	runModel(t, a, time.Now())
	r.offline.Store(false)
	r.imageBytes.Store(appImageBytes(t, color.RGBA{B: 255, A: 255}))
	runModel(t, a, time.Now().Add(time.Minute))
	if r.files.Load() != 1 || r.downloads.Load() != 1 || r.models.Load() != 2 || scalar(t, path, `SELECT count(DISTINCT request) FROM attempts`) != 1 {
		t.Fatal("model retry changed frozen image material")
	}
}

func TestImageHashCacheReusesIdenticalMaterialAndInvalidatesChangedBytes(t *testing.T) {
	a, path, r := imageApp(t)
	for i := 1; i <= 3; i++ {
		if i == 3 {
			r.imageBytes.Store(appImageBytes(t, color.RGBA{B: 255, A: 255}))
		}
		deliverRawMessage(t, a, int64(i), visualMessageRaw(i, "photo"))
		runModel(t, a, time.Now())
	}
	if r.files.Load() != 3 || r.models.Load() != 2 || scalar(t, path, `SELECT count(*) FROM content_cache_matches`) != 1 || scalar(t, path, `SELECT count(*) FROM revision_images`) != 3 {
		t.Fatal("image cache ignored pixels or lost matched-message material")
	}
}

func TestWrongModelNameDoesNotTriggerImageFallback(t *testing.T) {
	a, path, r := imageApp(t)
	r.rejectCode.Store(422)
	r.rejectField.Store("model")
	deliverRawMessage(t, a, 1, visualMessageRaw(1, "photo"))
	runModel(t, a, time.Now())
	if r.models.Load() != 1 || scalar(t, path, `SELECT count(*) FROM revisions WHERE status='pending'`) != 1 || scalar(t, path, `SELECT count(*) FROM meta WHERE key='model_retry_at'`) != 1 {
		t.Fatal("configuration failure was hidden as image fallback")
	}
}

func TestOldSegmentedPendingPhotoRetainsMetadataOnlyInput(t *testing.T) {
	a, path, r := imageApp(t)
	deliverRawMessage(t, a, 1, visualMessageRaw(1, "photo"))
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE revisions SET snapshot=json_set(snapshot,'$.context_version',2)`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	runModel(t, a, time.Now())
	if r.files.Load() != 0 || r.models.Load() != 1 || scalar(t, path, `SELECT count(*) FROM attempts WHERE json_type(request,'$.assets') IS NULL AND json_type(request,'$.state.message')='array'`) != 1 {
		t.Fatal("old pending job silently gained images or changed layout")
	}
	if _, err := a.Store.ImageAssets(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
}

func TestVisualDiagnosticsReportTheActualImageDeadline(t *testing.T) {
	a, path, _ := imageApp(t)
	var logs bytes.Buffer
	a.Log = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := a.Config.Snapshot()
	cfg.Base.Debug = true
	cfg.Model.TimeoutSeconds = 120
	cfg.Model.ImageTimeoutSeconds = 30
	saveScoringConfig(t, a, path, cfg)
	deliverRawMessage(t, a, 1, visualMessageRaw(1, "photo"))
	runModel(t, a, time.Now())
	for _, line := range bytes.Split(logs.Bytes(), []byte("\n")) {
		var event map[string]any
		if json.Unmarshal(line, &event) == nil && event["event"] == "model.started" {
			if event["timeout_seconds"] != float64(30) || event["image_count"] != float64(1) {
				t.Fatalf("wrong visual request diagnostics: %s", line)
			}
			return
		}
	}
	t.Fatal("model.started diagnostic missing")
}
