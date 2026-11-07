package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"telegram-antispam-bot/internal/vision"
)

type ImageLookup struct {
	Source     ImageSource     `json:"source"`
	Status     string          `json:"status"`
	Error      string          `json:"error,omitempty"`
	Request    json.RawMessage `json:"request,omitempty"`
	Response   json.RawMessage `json:"response,omitempty"`
	HTTPStatus int             `json:"http_status,omitempty"`
	FetchedAt  int64           `json:"fetched_at"`
	DurationMS int64           `json:"duration_ms"`
	Downloaded []byte          `json:"downloaded_bytes,omitempty"`
	SHA256     string          `json:"normalized_sha256,omitempty"`
	Width      int             `json:"width,omitempty"`
	Height     int             `json:"height,omitempty"`
	Reference  string          `json:"reference,omitempty"`
	Asset      vision.Asset    `json:"-"`
}

func imageHTTPClient(client bot.HttpClient) *http.Client {
	var base *http.Client
	switch c := client.(type) {
	case *http.Client:
		base = c
	case *DurableClient:
		base = c.HTTP
	}
	if base == nil {
		return nil
	}
	clone := *base
	clone.Timeout = 10 * time.Second
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &clone
}

func (c *Client) LookupImage(ctx context.Context, source ImageSource) (result ImageLookup) {
	start := time.Now()
	result.Source = source
	result.FetchedAt = start.UnixMilli()
	result.Status = "unavailable"
	defer func() { result.DurationMS = time.Since(start).Milliseconds() }()
	if source.FileID == "" {
		result.Error = "image source has no downloadable file ID"
		return result
	}
	if source.FileSize > vision.MaxImageBytes {
		result.Status = "limit"
		result.Error = "image exceeds download budget"
		return result
	}
	result.Request, _ = json.Marshal(map[string]any{"file_id": source.FileID})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	file, err := c.Bot.GetFile(ctx, &bot.GetFileParams{FileID: source.FileID})
	if err != nil {
		result.Error = c.safe(err).Error()
		return result
	}
	result.Response, _ = json.Marshal(file)
	// Local gateways may echo credential-bearing absolute paths. Retain the
	// parsed response with transport credentials removed before it is archived.
	for _, secret := range []string{c.token, url.PathEscape(c.token), url.QueryEscape(c.token), strings.ReplaceAll(url.QueryEscape(c.token), "%3A", "%3a")} {
		if secret != "" {
			result.Response = json.RawMessage(strings.ReplaceAll(string(result.Response), secret, "[redacted]"))
		}
	}
	if file == nil || file.FileID == "" || (source.FileUniqueID != "" && file.FileUniqueID != source.FileUniqueID) {
		result.Error = "returned file identity does not match image source"
		return result
	}
	p := file.FilePath
	if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") || path.Clean(p) != p || strings.ContainsAny(p, "\\?#%:\x00\r\n") {
		result.Error = "unsafe Telegram file path"
		return result
	}
	if file.FileSize > vision.MaxImageBytes {
		result.Status = "limit"
		result.Error = "image exceeds download budget"
		return result
	}
	if c.files == nil {
		result.Error = "image download requires redirect-controlled HTTP transport"
		return result
	}
	u, err := url.Parse(c.apiBase)
	if err != nil {
		result.Error = "invalid Telegram file endpoint"
		return result
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/file/bot" + c.token + "/" + p
	u.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		result.Error = "invalid Telegram file request"
		return result
	}
	resp, err := c.files.Do(req)
	if err != nil {
		result.Error = c.safe(err).Error()
		return result
	}
	defer resp.Body.Close()
	result.HTTPStatus = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		result.Error = fmt.Sprintf("Telegram file HTTP status %d", resp.StatusCode)
		return result
	}
	if resp.ContentLength > vision.MaxImageBytes {
		result.Status = "limit"
		result.Error = "image exceeds download budget"
		return result
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, vision.MaxImageBytes+1))
	if err != nil {
		result.Error = c.safe(err).Error()
		return result
	}
	if len(raw) > vision.MaxImageBytes {
		result.Status = "limit"
		result.Error = "image exceeds download budget"
		return result
	}
	im, err := vision.Normalize(raw)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Status = "available"
	result.Downloaded = raw
	result.SHA256 = im.SHA256
	result.Width = im.Width
	result.Height = im.Height
	result.Asset = im.Asset()
	return result
}

func (c *Client) MessageImages(ctx context.Context, m *Message) []ImageLookup {
	budget, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out []ImageLookup
	for i, source := range m.ImageSources() {
		if i >= 2 || budget.Err() != nil {
			out = append(out, ImageLookup{Source: source, Status: "limit", Error: "per-message image budget exhausted", FetchedAt: time.Now().UnixMilli()})
			continue
		}
		out = append(out, c.LookupImage(budget, source))
	}
	return out
}
