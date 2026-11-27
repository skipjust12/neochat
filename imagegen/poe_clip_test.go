package imagegen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"neochat/attachment"
)

func TestPoeLongVideoReferenceIsActuallyTrimmed(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	file := filepath.Join(t.TempDir(), "source.mp4")
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=32x32:r=10", "-t", "2", "-c:v", "libx264", "-threads", "1", file)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture ffmpeg: %v %s", err, output)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := clipPoeReference(ctx, Reference{MIME: "video/mp4", Data: data}, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	seconds, ok := attachment.VideoDuration(ref.Data)
	if ref.MIME != "video/mp4" || !ok || seconds < 0.4 || seconds > 0.7 {
		t.Fatalf("clip duration=%v valid=%v mime=%s", seconds, ok, ref.MIME)
	}
}
