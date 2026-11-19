package vision

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
)

const (
	maxGIFFrames      = 1000
	maxGIFFramePixels = 64 << 20
	maxCanvasPixels   = 16 << 20
)

// Frame is one normalized still taken from an animation. Position is
// "first", "last", or "single" when both ends render identically.
type Frame struct {
	Position string
	Image    Image
}

// ExtractFrames returns the first and last displayed frames of a GIF,
// composited in-process. Other containers, notably Telegram's MP4
// animations, are refused: decoding them would need a native video codec.
func ExtractFrames(ctx context.Context, raw []byte) ([]Frame, error) {
	if len(raw) > MaxImageBytes {
		return nil, fmt.Errorf("animation exceeds 4 MiB download budget")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(raw, []byte("GIF87a")) && !bytes.HasPrefix(raw, []byte("GIF89a")) {
		return nil, fmt.Errorf("animation is not a GIF")
	}
	first, last, err := gifEnds(raw)
	if err != nil {
		return nil, err
	}
	a, err := normalizeDecoded(first)
	if err != nil {
		return nil, err
	}
	b, err := normalizeDecoded(last)
	if err != nil {
		return nil, err
	}
	if a.SHA256 == b.SHA256 {
		return []Frame{{"single", a}}, nil
	}
	return []Frame{{"first", a}, {"last", b}}, nil
}

// scanGIF walks block structure without decompressing image data, so frame
// count and declared pixel totals are bounded before image/gif allocates.
func scanGIF(raw []byte) (frames int, pixels int64, err error) {
	bad := errors.New("malformed GIF structure")
	if len(raw) < 13 {
		return 0, 0, bad
	}
	p := 13
	if raw[10]&0x80 != 0 {
		p += 3 << ((raw[10] & 7) + 1)
	}
	skipBlocks := func() bool {
		for p < len(raw) {
			n := int(raw[p])
			p++
			if n == 0 {
				return true
			}
			p += n
		}
		return false
	}
	for p < len(raw) {
		switch raw[p] {
		case 0x21:
			p += 2
			if !skipBlocks() {
				return 0, 0, bad
			}
		case 0x2c:
			if p+10 > len(raw) {
				return 0, 0, bad
			}
			w := int64(binary.LittleEndian.Uint16(raw[p+5:]))
			h := int64(binary.LittleEndian.Uint16(raw[p+7:]))
			packed := raw[p+9]
			p += 10
			if packed&0x80 != 0 {
				p += 3 << ((packed & 7) + 1)
			}
			p++ // LZW minimum code size
			if !skipBlocks() {
				return 0, 0, bad
			}
			frames++
			pixels += w * h
			if frames > maxGIFFrames || pixels > maxGIFFramePixels {
				return 0, 0, fmt.Errorf("GIF exceeds frame or decoded-pixel budget")
			}
		case 0x3b:
			return frames, pixels, nil
		default:
			return 0, 0, bad
		}
	}
	return frames, pixels, nil
}

func gifEnds(raw []byte) (image.Image, image.Image, error) {
	cfg, err := gif.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("decode GIF header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxCanvasPixels {
		return nil, nil, fmt.Errorf("GIF canvas exceeds pixel budget")
	}
	if frames, _, err := scanGIF(raw); err != nil {
		return nil, nil, err
	} else if frames == 0 {
		return nil, nil, fmt.Errorf("GIF has no frames")
	}
	g, err := gif.DecodeAll(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("decode GIF: %w", err)
	}
	canvas := image.NewRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
	var first image.Image
	for i, frame := range g.Image {
		disposal := byte(0)
		if i < len(g.Disposal) {
			disposal = g.Disposal[i]
		}
		var saved *image.RGBA
		if disposal == gif.DisposalPrevious {
			saved = image.NewRGBA(canvas.Bounds())
			copy(saved.Pix, canvas.Pix)
		}
		draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Over)
		if i == 0 {
			snapshot := image.NewRGBA(canvas.Bounds())
			copy(snapshot.Pix, canvas.Pix)
			first = snapshot
		}
		if i == len(g.Image)-1 {
			break
		}
		switch disposal {
		case gif.DisposalBackground:
			draw.Draw(canvas, frame.Bounds(), image.Transparent, image.Point{}, draw.Src)
		case gif.DisposalPrevious:
			canvas = saved
		}
	}
	return first, canvas, nil
}
