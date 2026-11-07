package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"testing"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/telegram"
	"telegram-antispam-bot/internal/vision"
)

func visualJob(t *testing.T, s *Store, id int64, c color.Color) *Job {
	t.Helper()
	ctx := context.Background()
	var m telegram.Message
	raw, _ := json.Marshal(map[string]any{"message_id": id, "chat": map[string]any{"id": -100, "type": "supergroup"}, "from": map[string]any{"id": 100 + id, "first_name": "Member"}, "text": "ad", "photo": []map[string]any{{"file_id": "f", "file_unique_id": "u", "width": 320, "height": 280}}})
	json.Unmarshal(raw, &m)
	update, _ := json.Marshal(map[string]any{"update_id": id, "message": json.RawMessage(raw)})
	if err := s.Capture(ctx, []json.RawMessage{update}); err != nil {
		t.Fatal(err)
	}
	p := config.Policy{Mode: "record", QueueLimit: 10, Questions: []string{"content.ad"}, Condition: "p.content.ad >= 0.9", Rules: map[string]config.Rule{"delete": {Enabled: true, Count: 1, Action: "delete"}}}
	q := map[string]config.Question{"content.ad": {Type: "noul", Instructions: "Judge current text and image."}}
	report, err := s.AddRevision(ctx, Inbox{ID: id, Raw: update, ReceivedAt: testNow.UnixMilli()}, &m, Snapshot{ContextVersion: telegram.VisualContextVersion, Policy: p, Questions: q, ModelID: "m", ModelEndpoint: "http://127.0.0.1:8000"}, "pending")
	if err != nil || report.Cache != nil {
		t.Fatal("visual admission reused cache before image bytes")
	}
	j, err := s.Claim(ctx, testNow)
	if err != nil || j == nil {
		t.Fatal(err)
	}
	im := image.NewRGBA(image.Rect(0, 0, 320, 280))
	for y := 0; y < 280; y++ {
		for x := 0; x < 320; x++ {
			im.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	png.Encode(&b, im)
	normalized, err := vision.Normalize(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	lookups := []telegram.ImageLookup{{Source: telegram.ImageSource{Kind: "photo", FileID: "f", FileUniqueID: "u"}, Status: "available", Downloaded: b.Bytes(), SHA256: normalized.SHA256, Asset: normalized.Asset()}}
	state, err := m.SegmentedModelState(telegram.Profile{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, assets, err := telegram.AttachImages(state, lookups)
	if err != nil {
		t.Fatal(err)
	}
	j.State, j.Assets, err = s.FreezeVisualContext(ctx, j.ID, state, assets, lookups)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func visualResult(j *Job, images bool) decision.Result {
	state, assets := j.State, j.Assets
	if !images {
		state, _ = telegram.WithoutImages(state, assets)
		assets = nil
	}
	request, _ := json.Marshal(map[string]any{"model": "m", "state": json.RawMessage(state), "questions": j.Snapshot.Questions, "assets": assets})
	return decision.Result{ModelID: "m", ModelEndpoint: "http://127.0.0.1:8000", Request: request, Response: []byte(`{"answers":{"content.ad":{"type":"noul","noul":1}}}`), Probabilities: map[string]float64{"content.ad": 1}}
}

func TestVisualContextFreezesStateAssetsAndOriginalDownloadTogether(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := visualJob(t, s, 1, color.RGBA{R: 255, A: 255})
	state, assets, err := s.FreezeVisualContext(ctx, j.ID, []byte(`{"different":true}`), vision.Assets{"other": {}}, nil)
	if err != nil || !bytes.Equal(state, j.State) || !reflect.DeepEqual(assets, j.Assets) {
		t.Fatal("freeze replaced state or asset snapshot")
	}
	loaded, err := s.ImageAssets(ctx, j.ID)
	if err != nil || !reflect.DeepEqual(loaded, j.Assets) {
		t.Fatal("frozen assets unavailable on retry")
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM revision_images WHERE json_extract(lookups,'$[0].status')='available' AND json_extract(lookups,'$[0].downloaded_bytes')<>''`).Scan(&n)
	if n != 1 {
		t.Fatal("original image bytes not archived")
	}
}

func TestVisualCacheReusesEqualImagesAndSeparatesChangedPixels(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	red := color.RGBA{R: 255, A: 255}
	first := visualJob(t, s, 1, red)
	if _, err := s.Finish(ctx, *first, visualResult(first, true), nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
	second := visualJob(t, s, 2, red)
	if _, hit, err := s.ApplyContentCache(ctx, *second, config.Model{Name: "m", BaseURL: "http://127.0.0.1:8000"}); err != nil || !hit {
		t.Fatalf("identical visual content missed cache: %v %v", hit, err)
	}
	third := visualJob(t, s, 3, color.RGBA{B: 255, A: 255})
	if _, hit, err := s.ApplyContentCache(ctx, *third, config.Model{Name: "m", BaseURL: "http://127.0.0.1:8000"}); err != nil || hit {
		t.Fatal("changed pixels reused caption-only judgment")
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM content_cache_matches`).Scan(&n)
	if n != 1 {
		t.Fatal("cache hit audit missing")
	}
}

func TestTextFallbackIsAuditedWithoutPublishingVisualJudgment(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := visualJob(t, s, 1, color.Black)
	failed := visualResult(j, true)
	failed.Response = []byte(`{"detail":[{"loc":["body","state"],"msg":"vision unavailable"}]}`)
	if _, err := s.RecordModelFailure(ctx, j.ID, failed, errors.New("model HTTP status 422"), testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finish(ctx, *j, visualResult(j, false), nil, testNow, 10); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM attempts`).Scan(&n)
	if n != 2 {
		t.Fatal("image failure and text retry not independently archived")
	}
	s.db.QueryRow(`SELECT count(*) FROM content_cache`).Scan(&n)
	if n != 0 {
		t.Fatal("text-only fallback published image proof")
	}
	s.db.QueryRow(`SELECT count(*) FROM meta WHERE key='model_retry_at'`).Scan(&n)
	if n != 0 {
		t.Fatal("recoverable image rejection paused model queue")
	}
}
