package imagegen

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// Poe's file input has no video_start/video_end fields. Trim locally instead
// of sending unsupported Polza parameters and silently uploading a full clip.
func clipPoeReference(ctx context.Context, ref Reference, seconds float64) (Reference, error) {
	if seconds <= 0 {
		return ref, nil
	}
	dir, err := os.MkdirTemp("", "neochat-poe-video-")
	if err != nil {
		return Reference{}, err
	}
	defer os.RemoveAll(dir)
	input, output := filepath.Join(dir, "input"), filepath.Join(dir, "reference.mp4")
	if err = os.WriteFile(input, ref.Data, 0600); err != nil {
		return Reference{}, err
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-protocol_whitelist", "file,pipe", "-i", input, "-t", strconv.FormatFloat(seconds, 'f', 3, 64), "-map", "0:v:0", "-map", "0:a?", "-c:v", "libx264", "-threads", "2", "-preset", "veryfast", "-crf", "23", "-c:a", "aac", "-movflags", "+faststart", output)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Reference{}, ctx.Err()
		}
		return Reference{}, fmt.Errorf("could not trim the reference video; ffmpeg with libx264 is required: %w", err)
	}
	file, err := os.Open(output)
	if err != nil {
		return Reference{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, defaultMaxVideoBytes+1))
	if err != nil {
		return Reference{}, err
	}
	if len(data) > defaultMaxVideoBytes {
		return Reference{}, fmt.Errorf("trimmed video is too large")
	}
	return Reference{MIME: "video/mp4", Data: data}, nil
}
