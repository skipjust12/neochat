package attachment

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"path/filepath"
	"strings"

	// Decoders for every accepted image type; WebP is what the browser
	// now uploads.
	_ "image/gif"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Images are stored no larger than vendors actually use them: Claude
// downscales anything past ~1568 px on the long side, and GPT/Gemini work
// at similar sizes, so extra pixels only cost disk.
const (
	MaxImageSide = 1568

	// The browser already sends images at MaxImageSide as WebP; these
	// thresholds only catch uploads that skipped it (other clients, a
	// browser that couldn't decode the file), with a little slack so a
	// browser-prepared image is never re-encoded a second time.
	resizeAboveSide       = 1650
	recompressAboveBytes  = 1536 << 10
	imageJPEGQuality      = 82
	MaxImagePixels        = 40_000_000
	maxConcurrentReencode = 2
)

// ErrImageDimensions rejects images whose pixel count would make decoding
// them a memory bomb (a 1 MB file can declare 100k x 100k pixels).
var ErrImageDimensions = errors.New("image is too large to process")

// reencodeSlots bounds how many full-size images are decoded at once: a
// 40 MP PNG is ~160 MB in memory while it's being scaled.
var reencodeSlots = make(chan struct{}, maxConcurrentReencode)

// NormalizeImage shrinks an uploaded image to at most MaxImageSide on its
// long edge and re-encodes it: JPEG for opaque images, PNG when there is
// transparency (falling back to JPEG on white if that PNG would still be
// over MaxImageBytes). Images that are already small are returned as is.
// It returns the bytes to store and their MIME type.
func NormalizeImage(data []byte, mimeType string) ([]byte, string, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", ErrUnsupportedType
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > MaxImagePixels {
		return nil, "", fmt.Errorf("%w: images are limited to %d megapixels", ErrImageDimensions, MaxImagePixels/1_000_000)
	}
	long := max(config.Width, config.Height)
	if long <= resizeAboveSide && len(data) <= recompressAboveBytes {
		if len(data) > MaxImageBytes {
			return nil, "", ErrTooLarge
		}
		return data, mimeType, nil
	}

	reencodeSlots <- struct{}{}
	defer func() { <-reencodeSlots }()

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", ErrUnsupportedType
	}
	img := src
	if long > MaxImageSide {
		bounds := src.Bounds()
		width, height := bounds.Dx(), bounds.Dy()
		if width >= height {
			width, height = MaxImageSide, max(1, height*MaxImageSide/width)
		} else {
			width, height = max(1, width*MaxImageSide/height), MaxImageSide
		}
		scaled := image.NewNRGBA(image.Rect(0, 0, width, height))
		xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), src, bounds, xdraw.Src, nil)
		img = scaled
	}

	out, outMIME, err := encodeImage(img)
	if err != nil {
		return nil, "", err
	}
	// Re-encoding a small image that was only over the byte threshold can
	// come out bigger; keep whichever is smaller.
	if long <= resizeAboveSide && len(out) >= len(data) && len(data) <= MaxImageBytes {
		return data, mimeType, nil
	}
	if len(out) > MaxImageBytes {
		return nil, "", ErrTooLarge
	}
	return out, outMIME, nil
}

func encodeImage(img image.Image) ([]byte, string, error) {
	var buf bytes.Buffer
	if !isOpaque(img) {
		encoder := png.Encoder{CompressionLevel: png.DefaultCompression}
		if err := encoder.Encode(&buf, img); err != nil {
			return nil, "", err
		}
		if buf.Len() <= MaxImageBytes {
			return buf.Bytes(), "image/png", nil
		}
		// Too big as PNG: flatten the transparency onto white and use JPEG.
		flat := image.NewRGBA(img.Bounds())
		draw.Draw(flat, flat.Bounds(), image.White, image.Point{}, draw.Src)
		draw.Draw(flat, flat.Bounds(), img, img.Bounds().Min, draw.Over)
		img = flat
		buf.Reset()
	}
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: imageJPEGQuality}); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "image/jpeg", nil
}

func isOpaque(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return o.Opaque()
	}
	return false
}

// RenameForMIME swaps a file name's extension to match a re-encoded type,
// so "photo.webp" stored as JPEG is offered back as "photo.jpg".
func RenameForMIME(name, mimeType string) string {
	ext := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif"}[mimeType]
	if ext == "" {
		return name
	}
	current := strings.ToLower(filepath.Ext(name))
	if current == ext || (ext == ".jpg" && current == ".jpeg") {
		return name
	}
	return strings.TrimSuffix(name, filepath.Ext(name)) + ext
}
