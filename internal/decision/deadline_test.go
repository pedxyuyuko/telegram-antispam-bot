package decision

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/vision"
)

type deadlineTransport func(*http.Request) (*http.Response, error)

func (f deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestImageDeadlineCanBeShorterThanTextDeadline(t *testing.T) {
	want := 30 * time.Second
	c := &Client{HTTP: &http.Client{Transport: deadlineTransport(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		remaining := time.Until(deadline)
		if !ok || remaining > want || remaining < want-time.Second {
			t.Errorf("request deadline=%v want %v", remaining, want)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"model":"m","answers":{"q":{"type":"noul","noul":0.9}}}`)), Header: make(http.Header)}, nil
	})}}
	cfg := config.Defaults().Model
	cfg.Name = "m"
	cfg.TimeoutSeconds = 120
	cfg.ImageTimeoutSeconds = 30
	q := map[string]config.Question{"q": {Type: "noul", Instructions: "q"}}
	if _, err := c.EvaluateWithAssets(context.Background(), cfg, q, []byte(`{"image":"ref"}`), vision.Assets{"ref": testImageAsset(t)}); err != nil {
		t.Fatal(err)
	}
	want = 120 * time.Second
	if _, err := c.Evaluate(context.Background(), cfg, q, []byte(`"text"`)); err != nil {
		t.Fatal(err)
	}
}
