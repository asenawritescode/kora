package api

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestOptimizeUploadedImage_ConvertsOpaquePNGToSmallerJPEG(t *testing.T) {
	src := makeTestImage(256, 256, false)
	raw := encodePNG(t, src)

	optimized, mime, ok := optimizeUploadedImage(raw, "hero.png", "image/png")
	if !ok {
		t.Fatalf("expected optimization to apply")
	}
	if mime != "image/jpeg" {
		t.Fatalf("mime = %q, want image/jpeg", mime)
	}
	if len(optimized) >= len(raw) {
		t.Fatalf("optimized size = %d, want smaller than %d", len(optimized), len(raw))
	}

	decoded, format := decodeImage(t, optimized)
	if format != "jpeg" {
		t.Fatalf("format = %q, want jpeg", format)
	}
	if decoded.Bounds().Dx() != 256 || decoded.Bounds().Dy() != 256 {
		t.Fatalf("decoded bounds = %v, want 256x256", decoded.Bounds())
	}
}

func TestOptimizeUploadedImage_ConvertsJPEGToSmallerJPEG(t *testing.T) {
	src := makeTestImage(256, 256, false)
	raw := encodeJPEG(t, src, 100)

	optimized, mime, ok := optimizeUploadedImage(raw, "hero.jpg", "image/jpeg")
	if !ok {
		t.Fatalf("expected optimization to apply")
	}
	if mime != "image/jpeg" {
		t.Fatalf("mime = %q, want image/jpeg", mime)
	}
	if len(optimized) >= len(raw) {
		t.Fatalf("optimized size = %d, want smaller than %d", len(optimized), len(raw))
	}
}

func TestOptimizeUploadedImage_LeavesSVGAlone(t *testing.T) {
	raw := []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)
	optimized, mime, ok := optimizeUploadedImage(raw, "logo.svg", "image/svg+xml")
	if ok {
		t.Fatalf("expected no optimization for svg")
	}
	if mime != "image/svg+xml" {
		t.Fatalf("mime = %q, want image/svg+xml", mime)
	}
	if !bytes.Equal(optimized, raw) {
		t.Fatalf("svg bytes changed")
	}
}

func makeTestImage(w, h int, withAlpha bool) image.Image {
	if withAlpha {
		img := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				n := uint32(x*73856093 ^ y*19349663)
				img.SetNRGBA(x, y, color.NRGBA{
					R: uint8(n),
					G: uint8(n >> 8),
					B: uint8(n >> 16),
					A: 200,
				})
			}
		}
		return img
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			n := uint32(x*73856093 ^ y*19349663)
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(n),
				G: uint8(n >> 8),
				B: uint8(n >> 16),
				A: 255,
			})
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image, quality int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}
	return buf.Bytes()
}

func decodeImage(t *testing.T, raw []byte) (image.Image, string) {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return img, format
}
