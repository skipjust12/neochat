package attachment

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"testing"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// noisy fills img so it doesn't compress to almost nothing.
func noisy(width, height int, alpha uint8) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	seed := uint32(1)
	for i := 0; i < len(img.Pix); i += 4 {
		seed = seed*1664525 + 1013904223
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = uint8(seed>>8), uint8(seed>>16), uint8(seed>>24), alpha
	}
	return img
}

func TestNormalizeImageShrinksLargeOpaqueToJPEG(t *testing.T) {
	data := encodePNG(t, noisy(1800, 600, 0xff))
	out, mime, err := NormalizeImage(data, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/jpeg" || format != "jpeg" || config.Width != MaxImageSide || config.Height != 522 {
		t.Fatalf("got %s %s %dx%d, want image/jpeg %dx522", mime, format, config.Width, config.Height, MaxImageSide)
	}
	if len(out) >= len(data) {
		t.Fatalf("re-encoded %d bytes from %d", len(out), len(data))
	}
}

func TestNormalizeImageKeepsTransparency(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 700, 1800))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = uint8(i), uint8(i>>8), 90, uint8(i>>4)
	}
	out, mime, err := NormalizeImage(encodePNG(t, img), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	config, _, _ := image.DecodeConfig(bytes.NewReader(out))
	if mime != "image/png" || config.Height != MaxImageSide || config.Width != 609 {
		t.Fatalf("got %s %dx%d, want image/png 609x%d", mime, config.Width, config.Height, MaxImageSide)
	}
}

func TestNormalizeImageLeavesSmallImagesAlone(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, noisy(800, 600, 0xff), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	out, mime, err := NormalizeImage(buf.Bytes(), "image/jpeg")
	if err != nil || mime != "image/jpeg" || !bytes.Equal(out, buf.Bytes()) {
		t.Fatalf("a small image was changed: %s, %v", mime, err)
	}
}

func TestNormalizeImageRejectsDecompressionBomb(t *testing.T) {
	// A real 1x1 PNG whose header claims 20000x20000 pixels.
	data := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	ihdr := data[8+8 : 8+8+13] // signature, length+type, then IHDR data
	binary.BigEndian.PutUint32(ihdr[0:4], 20000)
	binary.BigEndian.PutUint32(ihdr[4:8], 20000)
	binary.BigEndian.PutUint32(data[8+8+13:], crc32.ChecksumIEEE(data[8+4:8+8+13]))
	if _, _, err := NormalizeImage(data, "image/png"); !errors.Is(err, ErrImageDimensions) {
		t.Fatalf("err = %v, want ErrImageDimensions", err)
	}
}

func TestNormalizeImageRejectsCorruptImage(t *testing.T) {
	if _, _, err := NormalizeImage(pngBytes, "image/png"); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("err = %v, want ErrUnsupportedType", err)
	}
}

func TestRenameForMIME(t *testing.T) {
	for _, tc := range [][3]string{
		{"photo.webp", "image/jpeg", "photo.jpg"},
		{"photo.JPEG", "image/jpeg", "photo.JPEG"},
		{"shot.png", "image/png", "shot.png"},
		{"noext", "image/jpeg", "noext.jpg"},
	} {
		if got := RenameForMIME(tc[0], tc[1]); got != tc[2] {
			t.Errorf("RenameForMIME(%q, %q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}
