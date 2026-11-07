package vision

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func pngFixture(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestNormalizeProducesBoundedStaticAssetsAndContentHash(t *testing.T) {
	for _, tc := range []struct{ w, h, wantW, wantH int }{{320, 280, 320, 280}, {2000, 500, 1024, 280}, {100, 2000, 280, 1024}, {40, 20, 280, 280}} {
		raw := pngFixture(t, tc.w, tc.h, color.NRGBA{R: 210, G: 30, B: 40, A: 255})
		i, err := Normalize(raw)
		if err != nil {
			t.Fatal(err)
		}
		if i.Width != tc.wantW || i.Height != tc.wantH || i.Format != "png" {
			t.Fatalf("normalized dimensions=%dx%d", i.Width, i.Height)
		}
		data, err := base64.StdEncoding.Strict().DecodeString(i.Asset().DataBase64)
		if err != nil {
			t.Fatal(err)
		}
		im, format, err := image.Decode(bytes.NewReader(data))
		if err != nil || format != "png" || im.Bounds().Dx() != tc.wantW || im.Bounds().Dy() != tc.wantH {
			t.Fatal("asset is not the normalized PNG")
		}
		if i.SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) {
			t.Fatal("hash is not bound to actual model bytes")
		}
		again, err := Normalize(raw)
		if err != nil || again.SHA256 != i.SHA256 {
			t.Fatal("normalization is nondeterministic")
		}
	}
}

func TestNormalizeConvertsWebPSticker(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString("UklGRigAAABXRUJQVlA4TBsAAAAvP8FFAAdQj0oXpf8BAUnS//9kRP8z/vN/4AEA")
	if err != nil {
		t.Fatal(err)
	}
	i, err := Normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(i.Data))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := im.At(160, 140).RGBA()
	if i.Width != 320 || i.Height != 280 || r>>8 != 210 || g>>8 != 30 || b>>8 != 40 {
		t.Fatal("WebP pixels changed")
	}
}

func jpegWithOrientation(t *testing.T, orientation byte) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 400, 280))
	for y := 0; y < 280; y++ {
		for x := 0; x < 400; x++ {
			c := color.RGBA{R: 255, A: 255}
			if x >= 200 {
				c = color.RGBA{B: 255, A: 255}
			}
			im.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, im, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	exif := []byte{'E', 'x', 'i', 'f', 0, 0, 'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 1, 3, 0, 1, 0, 0, 0, orientation, 0, 0, 0, 0, 0, 0, 0}
	out := []byte{0xff, 0xd8, 0xff, 0xe1, 0, byte(len(exif) + 2)}
	out = append(out, exif...)
	return append(out, b.Bytes()[2:]...)
}

func TestNormalizeAppliesExifToPixelsBeforeEncoding(t *testing.T) {
	i, err := Normalize(jpegWithOrientation(t, 6))
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(i.Data))
	if err != nil {
		t.Fatal(err)
	}
	if i.Width != 280 || i.Height != 400 {
		t.Fatalf("orientation not applied: %dx%d", i.Width, i.Height)
	}
	r, _, b, _ := im.At(140, 50).RGBA()
	if r < b {
		t.Fatal("top should be original left/red")
	}
	r, _, b, _ = im.At(140, 350).RGBA()
	if b < r {
		t.Fatal("bottom should be original right/blue")
	}
}

func TestOrientHandlesAllEightExifCases(t *testing.T) {
	im := image.NewRGBA(image.Rect(0, 0, 2, 2))
	colors := []color.RGBA{{R: 255, A: 255}, {G: 255, A: 255}, {B: 255, A: 255}, {R: 255, G: 255, A: 255}}
	for i, c := range colors {
		im.Set(i%2, i/2, c)
	}
	for n, want := range [][]int{{0, 1, 2, 3}, {1, 0, 3, 2}, {3, 2, 1, 0}, {2, 3, 0, 1}, {0, 2, 1, 3}, {2, 0, 3, 1}, {3, 1, 2, 0}, {1, 3, 0, 2}} {
		got := orient(im, n+1)
		for i, index := range want {
			if c := color.RGBAModel.Convert(got.At(i%2, i/2)).(color.RGBA); c != colors[index] {
				t.Fatalf("orientation=%d corner=%d got=%v", n+1, i, c)
			}
		}
	}
}

func TestNormalizeRejectsOversizedOrMalformedInputsBeforeDecode(t *testing.T) {
	raw := pngFixture(t, 2, 2, color.Black)
	bomb := append([]byte(nil), raw...)
	binary.BigEndian.PutUint32(bomb[16:20], 10000)
	binary.BigEndian.PutUint32(bomb[20:24], 10000)
	binary.BigEndian.PutUint32(bomb[29:33], crc32.ChecksumIEEE(bomb[12:29]))
	for _, raw := range [][]byte{[]byte("not an image"), make([]byte, (4<<20)+1), bomb} {
		if _, err := Normalize(raw); err == nil {
			t.Fatal("unsafe image accepted")
		}
	}
	for n := 0; n < 40; n++ {
		if _, err := Normalize(raw[:min(n, len(raw))]); err == nil {
			t.Fatal("truncated image accepted")
		}
	}
}
