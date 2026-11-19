// Package vision prepares bounded static images and SORS image assets.
package vision

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"math"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const MaxImageBytes = 4 << 20

type Asset struct {
	Type       string `json:"type"`
	Format     string `json:"format"`
	DataBase64 string `json:"data_base64"`
}
type Assets map[string]Asset

type Image struct {
	Data          []byte
	Format        string
	Width, Height int
	SHA256        string
}

func (i Image) Asset() Asset {
	return Asset{Type: "image", Format: i.Format, DataBase64: base64.StdEncoding.EncodeToString(i.Data)}
}

// Normalize checks dimensions before pixel allocation, applies source EXIF to
// pixels, and emits an opaque PNG with each edge in the service's legal range.
func Normalize(raw []byte) (Image, error) {
	if len(raw) > MaxImageBytes {
		return Image{}, fmt.Errorf("image exceeds 4 MiB download budget")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return Image{}, fmt.Errorf("decode image header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 16384 || cfg.Height > 16384 || int64(cfg.Width)*int64(cfg.Height) > 16<<20 {
		return Image{}, fmt.Errorf("image exceeds decoded pixel budget")
	}
	im, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return Image{}, fmt.Errorf("decode image: %w", err)
	}
	return normalizeDecoded(orient(im, exifOrientation(raw, format)))
}

// normalizeDecoded flattens transparency onto white and fits every edge into
// the SORS range. Frame extraction shares it after its own decode checks.
func normalizeDecoded(im image.Image) (Image, error) {
	w, h := im.Bounds().Dx(), im.Bounds().Dy()
	longest := max(w, h)
	scale := 1.0
	if longest > 1024 {
		scale = 1024 / float64(longest)
	} else if longest < 280 {
		scale = 280 / float64(longest)
	}
	w = max(1, int(math.Round(float64(w)*scale)))
	h = max(1, int(math.Round(float64(h)*scale)))
	canvas := image.NewRGBA(image.Rect(0, 0, max(w, 280), max(h, 280)))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	x, y := (canvas.Bounds().Dx()-w)/2, (canvas.Bounds().Dy()-h)/2
	xdraw.CatmullRom.Scale(canvas, image.Rect(x, y, x+w, y+h), im, im.Bounds(), draw.Over, nil)
	var out bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&out, canvas); err != nil {
		return Image{}, err
	}
	if out.Len() > MaxImageBytes {
		return Image{}, fmt.Errorf("normalized image exceeds 4 MiB budget")
	}
	data := out.Bytes()
	return Image{Data: data, Format: "png", Width: canvas.Bounds().Dx(), Height: canvas.Bounds().Dy(), SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}, nil
}

func orient(src image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return src
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	w, h := sw, sh
	if orientation >= 5 {
		w, h = h, w
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sx, sy := x, y
			switch orientation {
			case 2:
				sx = sw - 1 - x
			case 3:
				sx, sy = sw-1-x, sh-1-y
			case 4:
				sy = sh - 1 - y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, sh-1-x
			case 7:
				sx, sy = sw-1-y, sh-1-x
			case 8:
				sx, sy = sw-1-y, x
			}
			dst.Set(x, y, src.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return dst
}
