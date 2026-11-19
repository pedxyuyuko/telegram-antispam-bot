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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxGIFFrames      = 1000
	maxGIFFramePixels = 64 << 20
	maxCanvasPixels   = 16 << 20
	ffmpegTimeout     = 8 * time.Second
)

// Frame is one normalized still taken from an animation. Position is
// "first", "last", or "single" when both ends render identically.
type Frame struct {
	Position string
	Image    Image
}

// ExtractFrames returns the first and last displayed frames of an animation.
// GIF files are composited in-process; other containers (Telegram animations
// are MP4) are decoded by ffmpeg with input-only file access and a deadline.
func ExtractFrames(ctx context.Context, raw []byte, ffmpeg, scratch string) ([]Frame, error) {
	if len(raw) > MaxImageBytes {
		return nil, fmt.Errorf("animation exceeds 4 MiB download budget")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var first, last image.Image
	var err error
	if bytes.HasPrefix(raw, []byte("GIF87a")) || bytes.HasPrefix(raw, []byte("GIF89a")) {
		first, last, err = gifEnds(raw)
	} else {
		first, last, err = ffmpegEnds(ctx, raw, ffmpeg, scratch)
	}
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

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func ffmpegEnds(ctx context.Context, raw []byte, ffmpeg, scratch string) (image.Image, image.Image, error) {
	if ffmpeg == "" {
		path, err := exec.LookPath("ffmpeg")
		if err != nil {
			return nil, nil, fmt.Errorf("ffmpeg unavailable for animation frames")
		}
		ffmpeg = path
	}
	if scratch == "" {
		scratch = os.TempDir()
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp(scratch, "frames-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "input")
	if err := os.WriteFile(input, raw, 0o600); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, ffmpegTimeout)
	defer cancel()
	// Scale inside ffmpeg so the intermediate PNGs stay small; the shared
	// normalizer then applies the final SORS dimensions.
	scale := "scale=w='min(1024,iw)':h='min(1024,ih)':force_original_aspect_ratio=decrease"
	output := []string{"-map", "0:v:0", "-an", "-sn", "-dn", "-vf", scale, "-f", "image2", "-c:v", "png"}
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file",
		"-max_pixels", fmt.Sprint(maxCanvasPixels), "-i", input}
	args = append(append(args, output...), "-frames:v", "1", filepath.Join(dir, "first.png"))
	args = append(append(args, output...), "-update", "1", filepath.Join(dir, "last.png"))
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.Dir = dir
	cmd.Env = []string{}
	cmd.WaitDelay = time.Second
	stderr := &limitedBuffer{limit: 512}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("ffmpeg frame extraction stopped: %w", ctx.Err())
		}
		return nil, nil, fmt.Errorf("ffmpeg frame extraction failed: %v %s", err, detail)
	}
	read := func(name string) (image.Image, error) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || len(data) == 0 {
			return nil, fmt.Errorf("ffmpeg produced no %s", name)
		}
		if len(data) > MaxImageBytes {
			return nil, fmt.Errorf("%s exceeds 4 MiB budget", name)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || int64(cfg.Width)*int64(cfg.Height) > maxCanvasPixels {
			return nil, fmt.Errorf("invalid %s", name)
		}
		im, _, err := image.Decode(bytes.NewReader(data))
		return im, err
	}
	first, err := read("first.png")
	if err != nil {
		return nil, nil, err
	}
	last, err := read("last.png")
	if err != nil {
		return nil, nil, err
	}
	return first, last, nil
}
