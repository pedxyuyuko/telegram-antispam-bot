package decision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/vision"
)

func testImageAsset(t *testing.T) vision.Asset {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 280, 280))); err != nil {
		t.Fatal(err)
	}
	return vision.Asset{Type: "image", Format: "png", DataBase64: base64.StdEncoding.EncodeToString(b.Bytes())}
}

func TestSORSAssetsUseTopLevelMapAndReferencesOnlyInState(t *testing.T) {
	asset := testImageAsset(t)
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("wrong protocol endpoint or content type")
		}
		if calls == 1 {
			var assets vision.Assets
			if err := json.Unmarshal(body["assets"], &assets); err != nil {
				t.Error(err)
			}
			if len(body) != 4 || assets["image-ref"] != asset || r.Header.Get("TypeSafe-Danger-Zone-Images") != "true" || !bytes.Contains(body["state"], []byte("image-ref")) || bytes.Contains(body["state"], []byte("data:image")) {
				t.Error("asset protocol shape changed")
			}
		} else if len(body) != 3 || body["assets"] != nil || r.Header.Get("TypeSafe-Danger-Zone-Images") != "" {
			t.Error("text-only request gained image fields")
		}
		w.Write([]byte(`{"model":"m","answers":{"q":{"type":"noul","noul":0.9}},"usage":{"input_tokens":1024,"output_tokens":1}}`))
	}))
	defer s.Close()
	c := &Client{HTTP: s.Client()}
	cfg := config.Defaults().Model
	cfg.Name = "m"
	cfg.BaseURL = s.URL
	q := map[string]config.Question{"q": {Type: "noul", Instructions: "Look at the current message."}}
	r, err := c.EvaluateWithAssets(context.Background(), cfg, q, []byte(`{"message":[{"type":"media","media":[{"type":"photo","image":"image-ref"}]}]}`), vision.Assets{"image-ref": asset})
	if err != nil || r.Probabilities["q"] != .9 || !bytes.Contains(r.Response, []byte("usage")) {
		t.Fatalf("result=%+v %v", r, err)
	}
	if _, err := c.Evaluate(context.Background(), cfg, q, []byte(`{"message":"plain"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestImageRejectionDistinguishesImageFailuresFromConfigurationAndOutages(t *testing.T) {
	for _, tc := range []struct {
		code     int
		body     string
		fallback bool
	}{
		{400, `{"detail":"invalid image"}`, true}, {413, `{"detail":"image too large"}`, true},
		{422, `{"detail":[{"loc":["body","state"],"msg":"vision encoder unavailable","type":"value_error"}]}`, true},
		{422, `{"detail":[{"loc":["body","assets","ref"],"msg":"invalid asset","type":"value_error"}]}`, true},
		{422, `{"detail":[{"loc":["body","questions"],"msg":"state exceeds max token limit","type":"value_error"}]}`, true},
		{422, `{"detail":[{"loc":["body","model"],"msg":"wrong model","type":"value_error"}]}`, false},
		{422, `{"detail":[{"loc":["body","questions","q"],"msg":"invalid question","type":"value_error"}]}`, false},
		{401, `{"detail":"missing or invalid API key"}`, false}, {503, `{"detail":"unavailable"}`, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.code); w.Write([]byte(tc.body)) }))
			defer s.Close()
			cfg := config.Defaults().Model
			cfg.Name = "m"
			cfg.BaseURL = s.URL
			c := &Client{HTTP: s.Client()}
			q := map[string]config.Question{"q": {Type: "noul", Instructions: "q"}}
			r, err := c.EvaluateWithAssets(context.Background(), cfg, q, []byte(`{"image":"ref"}`), vision.Assets{"ref": testImageAsset(t)})
			if err == nil || ImageRejected(err) != tc.fallback || string(r.Response) != tc.body || strings.Contains(err.Error(), tc.body) {
				t.Fatalf("fallback=%v error=%v response=%s", ImageRejected(err), err, r.Response)
			}
			_, err = c.Evaluate(context.Background(), cfg, q, []byte(`"text"`))
			if ImageRejected(err) {
				t.Fatal("text failure classified as image rejection")
			}
		})
	}
}

func TestModelCallsAreSerializedAndWaitingRequestCanCancel(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		w.Write([]byte(`{"model":"m","answers":{"q":{"type":"noul","noul":0.9}}}`))
	}))
	defer s.Close()
	c := &Client{HTTP: s.Client()}
	cfg := config.Defaults().Model
	cfg.Name = "m"
	cfg.BaseURL = s.URL
	q := map[string]config.Question{"q": {Type: "noul", Instructions: "q"}}
	first := make(chan error, 1)
	go func() { _, err := c.Evaluate(context.Background(), cfg, q, []byte(`"first"`)); first <- err }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { _, err := c.Evaluate(ctx, cfg, q, []byte(`"second"`)); second <- err }()
	select {
	case err := <-second:
		close(release)
		<-first
		t.Fatalf("second request bypassed serialized slot: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waiting cancellation=%v", err)
		}
	case <-time.After(time.Second):
		t.Error("waiting request ignored cancellation")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("queued cancelled request reached model")
	}
}
