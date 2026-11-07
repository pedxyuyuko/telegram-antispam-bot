package telegram

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"telegram-antispam-bot/internal/vision"
)

const VisualContextVersion = 3

type ImageSource struct {
	Kind         string `json:"kind"`
	Preview      bool   `json:"preview,omitempty"`
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

// ImageSources covers the current message, including its forwarded media.
// Reply attachments retain their existing metadata-only context.
func (m *Message) ImageSources() []ImageSource {
	var fields map[string]json.RawMessage
	if json.Unmarshal(m.Bytes(), &fields) != nil {
		return nil
	}
	var sources []ImageSource
	if raw, ok := fields["photo"]; ok {
		var sizes []ImageSource
		_ = json.Unmarshal(raw, &sizes)
		chosen := ImageSource{Kind: "photo"}
		for _, size := range sizes {
			if size.FileID == "" || size.Width <= 0 || size.Height <= 0 || size.FileSize > vision.MaxImageBytes {
				continue
			}
			fit, bestFit := max(size.Width, size.Height) <= 1024, max(chosen.Width, chosen.Height) <= 1024
			area, bestArea := int64(size.Width)*int64(size.Height), int64(chosen.Width)*int64(chosen.Height)
			if chosen.FileID == "" || fit && !bestFit || fit == bestFit && (fit && area > bestArea || !fit && area < bestArea) {
				chosen = size
				chosen.Kind = "photo"
				chosen.Preview = false
			}
		}
		sources = append(sources, chosen)
	}
	if raw, ok := fields["sticker"]; ok {
		var s struct {
			ImageSource
			Animated  bool         `json:"is_animated"`
			Video     bool         `json:"is_video"`
			Thumbnail *ImageSource `json:"thumbnail"`
			Thumb     *ImageSource `json:"thumb"`
		}
		_ = json.Unmarshal(raw, &s)
		source := s.ImageSource
		source.Kind = "sticker"
		source.Preview = false
		if s.Animated || s.Video {
			source = ImageSource{Kind: "sticker", Preview: true}
			thumb := s.Thumbnail
			if thumb == nil {
				thumb = s.Thumb
			}
			if thumb != nil {
				source = *thumb
				source.Kind = "sticker"
				source.Preview = true
			}
		}
		sources = append(sources, source)
	}
	return sources
}

func newImageReference(state []byte, assets vision.Assets) (string, error) {
	for {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		b[6] = (b[6] & 15) | 64
		b[8] = (b[8] & 63) | 128
		ref := fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
		if _, exists := assets[ref]; !exists && !strings.Contains(string(state), ref) {
			return ref, nil
		}
	}
}

func currentMedia(state json.RawMessage) (map[string]any, []map[string]any, error) {
	var root map[string]any
	if !json.Valid(state) {
		return nil, nil, fmt.Errorf("invalid model state JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(state))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, nil, err
	}
	var media []map[string]any
	parts, _ := root["message"].([]any)
	for _, part := range parts {
		p, ok := part.(map[string]any)
		if !ok || p["type"] != "media" {
			continue
		}
		items, _ := p["media"].([]any)
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				media = append(media, m)
			}
		}
	}
	return root, media, nil
}

// AttachImages mutates only newly decoded current-message media and records the
// generated references in the lookup audit. Original Telegram JSON is untouched.
func AttachImages(state json.RawMessage, lookups []ImageLookup) (json.RawMessage, vision.Assets, error) {
	root, media, err := currentMedia(state)
	if err != nil {
		return nil, nil, err
	}
	assets := vision.Assets{}
	for i := range lookups {
		lookup := &lookups[i]
		if lookup.Status != "available" {
			continue
		}
		for _, item := range media {
			if item["type"] != lookup.Source.Kind {
				continue
			}
			if _, exists := item["image"]; exists {
				continue
			}
			ref, err := newImageReference(state, assets)
			if err != nil {
				return nil, nil, err
			}
			assets[ref] = lookup.Asset
			item["image"] = ref
			if lookup.Source.Preview {
				item["image_source"] = "thumbnail"
			}
			lookup.Reference = ref
			break
		}
	}
	raw, err := marshalStateOrder(state, root)
	return raw, assets, err
}

// WithoutImages removes only the generated image-reference fields, retaining
// all text, biographies, replies, provenance and other media metadata.
func WithoutImages(state json.RawMessage, assets vision.Assets) (json.RawMessage, error) {
	root, media, err := currentMedia(state)
	if err != nil {
		return nil, err
	}
	for _, item := range media {
		if ref, ok := item["image"].(string); ok {
			if _, known := assets[ref]; known {
				delete(item, "image")
				if item["image_source"] == "thumbnail" {
					delete(item, "image_source")
				}
			}
		}
	}
	return marshalStateOrder(state, root)
}

// VisualContentFingerprint includes the bytes actually supplied to the model,
// with randomized asset references replaced by format and SHA256. Missing image
// material cannot hit or publish an image judgment, even when captions match.
func (m *Message) VisualContentFingerprint(state json.RawMessage, assets vision.Assets) (ContentFingerprint, error) {
	sources := m.ImageSources()
	if len(sources) > 0 {
		if len(state) == 0 || len(assets) == 0 {
			return ContentFingerprint{}, nil
		}
		root, media, err := currentMedia(state)
		if err != nil {
			return ContentFingerprint{}, err
		}
		needed := map[string]int{}
		for _, s := range sources {
			needed[s.Kind]++
		}
		used := map[string]bool{}
		for _, item := range media {
			kind, _ := item["type"].(string)
			if needed[kind] == 0 {
				continue
			}
			ref, _ := item["image"].(string)
			asset, ok := assets[ref]
			if !ok || asset.Type != "image" || (asset.Format != "png" && asset.Format != "jpeg" && asset.Format != "jpg") {
				return ContentFingerprint{}, nil
			}
			if len(asset.DataBase64) > base64.StdEncoding.EncodedLen(vision.MaxImageBytes) {
				return ContentFingerprint{}, nil
			}
			data, err := base64.StdEncoding.Strict().DecodeString(asset.DataBase64)
			if err != nil || len(data) == 0 {
				return ContentFingerprint{}, nil
			}
			item["image"] = map[string]any{"format": asset.Format, "sha256": fmt.Sprintf("%x", sha256.Sum256(data))}
			needed[kind]--
			used[ref] = true
		}
		for _, n := range needed {
			if n != 0 {
				return ContentFingerprint{}, nil
			}
		}
		if len(used) != len(assets) {
			return ContentFingerprint{}, nil
		}
		state, err = json.Marshal(root)
		if err != nil {
			return ContentFingerprint{}, err
		}
	} else if len(assets) > 0 {
		return ContentFingerprint{}, nil
	}
	base, err := m.SegmentedContentFingerprint(state)
	if err != nil || base.Hash == "" {
		return base, err
	}
	raw, err := json.Marshal(struct {
		Version  int             `json:"context_version"`
		Material json.RawMessage `json:"material"`
	}{VisualContextVersion, base.Material})
	if err != nil {
		return ContentFingerprint{}, err
	}
	return ContentFingerprint{Hash: fmt.Sprintf("%x", sha256.Sum256(raw)), Material: raw}, nil
}
