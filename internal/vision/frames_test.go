package vision

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/png"
	"testing"
	"time"
)

var (
	red  = color.RGBA{R: 255, A: 255}
	blue = color.RGBA{B: 255, A: 255}
)

func gifFrame(rect image.Rectangle, c color.Color) *image.Paletted {
	frame := image.NewPaletted(rect, palette.Plan9)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			frame.Set(x, y, c)
		}
	}
	return frame
}

func encodeGIF(t *testing.T, g *gif.GIF) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, g); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func pixel(t *testing.T, im Image, x, y int) color.RGBA {
	t.Helper()
	decoded, err := png.Decode(bytes.NewReader(im.Data))
	if err != nil {
		t.Fatal(err)
	}
	return color.RGBAModel.Convert(decoded.At(x, y)).(color.RGBA)
}

func near(c, want color.RGBA) bool {
	d := func(a, b uint8) int { return max(int(a)-int(b), int(b)-int(a)) }
	return d(c.R, want.R) < 40 && d(c.G, want.G) < 40 && d(c.B, want.B) < 40
}

func TestGIFFramesCompositeFirstAndLastCanvas(t *testing.T) {
	canvas := image.Rect(0, 0, 320, 280)
	// The second frame covers only the top-left corner; the last visible
	// canvas must still contain the first frame elsewhere.
	raw := encodeGIF(t, &gif.GIF{
		Image:    []*image.Paletted{gifFrame(canvas, red), gifFrame(image.Rect(0, 0, 100, 100), blue)},
		Delay:    []int{10, 10},
		Disposal: []byte{gif.DisposalNone, gif.DisposalNone},
		Config:   image.Config{Width: 320, Height: 280},
	})
	frames, err := ExtractFrames(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || frames[0].Position != "first" || frames[1].Position != "last" {
		t.Fatalf("frames=%+v", frames)
	}
	if !near(pixel(t, frames[0].Image, 50, 50), red) || !near(pixel(t, frames[0].Image, 250, 200), red) {
		t.Fatal("first frame is not the initial canvas")
	}
	if !near(pixel(t, frames[1].Image, 50, 50), blue) || !near(pixel(t, frames[1].Image, 250, 200), red) {
		t.Fatal("last frame lost compositing of earlier frames")
	}
}

func TestGIFDisposalBackgroundClearsPreviousFrameRegion(t *testing.T) {
	canvas := image.Rect(0, 0, 320, 280)
	raw := encodeGIF(t, &gif.GIF{
		Image:    []*image.Paletted{gifFrame(canvas, red), gifFrame(image.Rect(200, 200, 260, 260), blue)},
		Delay:    []int{10, 10},
		Disposal: []byte{gif.DisposalBackground, gif.DisposalNone},
		Config:   image.Config{Width: 320, Height: 280},
	})
	frames, err := ExtractFrames(context.Background(), raw)
	if err != nil || len(frames) != 2 {
		t.Fatalf("frames=%+v err=%v", frames, err)
	}
	// Cleared regions become transparent and are flattened onto white.
	if c := pixel(t, frames[1].Image, 50, 50); !near(c, color.RGBA{255, 255, 255, 255}) {
		t.Fatalf("background disposal ignored: %v", c)
	}
	if !near(pixel(t, frames[1].Image, 230, 230), blue) {
		t.Fatal("last frame content missing")
	}
}

func TestSingleFrameGIFProducesOneFrame(t *testing.T) {
	raw := encodeGIF(t, &gif.GIF{Image: []*image.Paletted{gifFrame(image.Rect(0, 0, 320, 280), red)}, Delay: []int{0}})
	frames, err := ExtractFrames(context.Background(), raw)
	if err != nil || len(frames) != 1 || frames[0].Position != "single" {
		t.Fatalf("frames=%+v err=%v", frames, err)
	}
}

func TestGIFDecodedPixelBudgetIsCheckedBeforeDecoding(t *testing.T) {
	// Five frames each declaring a full 4096x4096 rectangle (80 Mi pixels in
	// total) while carrying almost no data: rejection must come from the
	// block scan, before image/gif allocates any frame.
	raw := []byte("GIF89a")
	raw = append(raw, 0x00, 0x10, 0x00, 0x10, 0x00, 0x00, 0x00)
	for i := 0; i < 5; i++ {
		raw = append(raw, 0x2c, 0, 0, 0, 0, 0x00, 0x10, 0x00, 0x10, 0x00, 0x02, 0x01, 0x00, 0x00)
	}
	raw = append(raw, 0x3b)
	started := time.Now()
	if _, err := ExtractFrames(context.Background(), raw); err == nil {
		t.Fatal("GIF exceeding the total decoded-pixel budget was decoded")
	}
	if time.Since(started) > time.Second {
		t.Fatal("budget check happened after expensive decoding")
	}
}

func TestNonGIFAnimationIsRejectedWithoutExternalDecoder(t *testing.T) {
	// An MP4 header (ftyp box) or any other container is refused outright;
	// only GIF is decoded, in-process.
	mp4 := append([]byte{0, 0, 0, 0x18}, []byte("ftypisom\x00\x00\x02\x00isomiso2")...)
	for _, raw := range [][]byte{mp4, []byte("not a video")} {
		if _, err := ExtractFrames(context.Background(), raw); err == nil {
			t.Fatalf("non-GIF animation accepted: %q", raw)
		}
	}
	if _, err := ExtractFrames(context.Background(), make([]byte, MaxImageBytes+1)); err == nil {
		t.Fatal("oversized animation accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ExtractFrames(ctx, encodeGIF(t, &gif.GIF{Image: []*image.Paletted{gifFrame(image.Rect(0, 0, 320, 280), red)}, Delay: []int{0}})); err == nil {
		t.Fatal("cancelled extraction reported success")
	}
}
