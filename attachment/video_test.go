package attachment

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func box(boxType string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], boxType)
	return append(out, payload...)
}

// mp4 builds an ISO media file of the given brand whose movie header says
// it lasts duration/timescale seconds, with the moov box after the media
// data, the way phones write it.
func mp4(brand string, version byte, timescale uint32, duration uint64) []byte {
	ftyp := box("ftyp", append([]byte(brand), 0, 0, 0, 0))
	var mvhd []byte
	if version == 0 {
		mvhd = make([]byte, 100)
		binary.BigEndian.PutUint32(mvhd[12:], timescale)
		binary.BigEndian.PutUint32(mvhd[16:], uint32(duration))
	} else {
		mvhd = make([]byte, 112)
		mvhd[0] = 1
		binary.BigEndian.PutUint32(mvhd[20:], timescale)
		binary.BigEndian.PutUint64(mvhd[24:], duration)
	}
	moov := box("moov", append(box("mvhd", mvhd), box("trak", make([]byte, 40))...))
	mdat := box("mdat", make([]byte, 4096))
	return append(append(ftyp, mdat...), moov...)
}

func TestVideoMIME(t *testing.T) {
	webm := append([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x9F, 0x42, 0x86, 0x81, 0x01, 0x42, 0x82, 0x84}, []byte("webm")...)
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"mp4", mp4("isom", 0, 1000, 5000), "video/mp4"},
		{"mp4 v2", mp4("mp42", 0, 1000, 5000), "video/mp4"},
		{"quicktime", mp4("qt  ", 0, 600, 6000), "video/quicktime"},
		{"3gp", mp4("3gp5", 0, 1000, 5000), "video/3gpp"},
		{"heic photo", mp4("heic", 0, 1000, 5000), ""},
		{"avif photo", mp4("avif", 0, 1000, 5000), ""},
		{"m4a audio", mp4("M4A ", 0, 1000, 5000), ""},
		{"webm", webm, "video/webm"},
		{"text", []byte("hello world, not a video"), ""},
	}
	for _, tc := range cases {
		if got := VideoMIME(tc.data); got != tc.want {
			t.Errorf("%s: VideoMIME = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestClassifyVideo(t *testing.T) {
	kind, mime, err := Classify("clip.mov", mp4("qt  ", 0, 600, 6000))
	if err != nil || kind != KindVideo || mime != "video/quicktime" {
		t.Fatalf("Classify(mov) = %q %q %v", kind, mime, err)
	}
	big := mp4("isom", 0, 1000, 5000)
	big = append(big, make([]byte, MaxVideoBytes)...)
	if _, _, err := Classify("big.mp4", big); !errors.Is(err, ErrTooLarge) || !strings.HasSuffix(err.Error(), "video files are limited to 50 MB") {
		t.Fatalf("oversized video: err = %v", err)
	}
	// A PDF the size of a big video is still over the document limit.
	pdf := append([]byte("%PDF-1.7\n"), make([]byte, MaxDocumentBytes)...)
	if _, _, err := Classify("big.pdf", pdf); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized PDF: err = %v, want ErrTooLarge", err)
	}
}

func TestVideoDuration(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want float64
		ok   bool
	}{
		{"v0", mp4("isom", 0, 1000, 12500), 12.5, true},
		{"v1", mp4("mp42", 1, 90000, 90000*75), 75, true},
		{"quicktime", mp4("qt  ", 0, 600, 600*3), 3, true},
		{"no duration", mp4("isom", 0, 1000, 0), 0, false},
		{"truncated", mp4("isom", 0, 1000, 5000)[:200], 0, false},
		{"not mp4", []byte("plain text"), 0, false},
	}
	for _, tc := range cases {
		got, ok := VideoDuration(tc.data)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: VideoDuration = %v %v, want %v %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
