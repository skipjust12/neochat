package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"neochat/conversation"
	"neochat/device"
	"neochat/provider"
)

// Viewing is private to the active model context: no attachment is stored or sent.
var viewImageTool = provider.FunctionTool{
	Name:        "view_image",
	Description: "Open an existing PNG/JPEG/WebP/GIF from the terminal so YOU can inspect it visually. The image is returned to you, not displayed or sent to the user. After rendering an image, inspect it here, fix clipping, labels or layout if needed, then use show_image only for the final picture the user should see.",
	Parameters:  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string", "description": "Image path on the selected terminal, relative to this chat's working directory or absolute."}}, "required": []string{"path"}},
}

func (r *webRun) planViewImage(call provider.ToolCall, position int) (string, func(context.Context) string) {
	if !r.terminalAllowed() || !modelAccepts(r.model, "image") {
		return "Error: viewing images is not available for this model or terminal.", nil
	}
	var args struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(call.Arguments), &args) != nil || strings.TrimSpace(args.Path) == "" {
		return "Error: path is required.", nil
	}
	if !r.take(&r.views, 10) {
		return "Error: image viewing limit reached (10 images per answer).", nil
	}
	index := r.start(conversation.ToolActivity{Tool: "view_image", Command: clipRunes(args.Path, 1000)})
	return "", func(ctx context.Context) string {
		data, name, err := r.readImageFile(ctx, args.Path)
		if err == nil {
			_, err = validateInlineImage(data)
		}
		r.finish(index, func(a *conversation.ToolActivity) {
			if err != nil {
				a.Error = err.Error()
			} else {
				a.Title = name
				a.Size = int64(len(data))
			}
		})
		if err != nil {
			return "Error: " + err.Error()
		}
		_, mime := sentFileKind(name, data)
		r.mu.Lock()
		if r.viewImages == nil {
			r.viewImages = map[int]provider.Part{}
		}
		r.viewImages[position] = provider.Part{Type: provider.PartImage, Name: name, MIME: mime, Data: data}
		r.mu.Unlock()
		return "Opened " + name + " for your visual inspection. The user has not been shown this image. Inspect it, correct problems if needed, and continue; show_image is a separate action."
	}
}

func (r *webRun) readImageFile(ctx context.Context, path string) ([]byte, string, error) {
	if r.onComputer() {
		path = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(path), "sandbox:"), "file://")
		res, err := r.runOnComputer(ctx, device.Request{Op: device.OpFetchFile, Path: path})
		if err != nil {
			return nil, "", err
		}
		if len(res.Data) > maxSentFileBytes {
			return nil, "", fmt.Errorf("image exceeds %s", sizeText(maxSentFileBytes))
		}
		return res.Data, fileName(res.Name, res.Path), nil
	}
	dir, err := r.s.Terminal.Dir(r.req.UserID, r.prepared.conversationID)
	if err != nil {
		return nil, "", err
	}
	real, err := resolveTerminalPath(dir, path)
	if err != nil {
		return nil, "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("image must be a regular file")
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("image must be a regular file")
	}
	if info.Size() > maxSentFileBytes {
		return nil, "", fmt.Errorf("image exceeds %s", sizeText(maxSentFileBytes))
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSentFileBytes+1))
	if err == nil && len(data) > maxSentFileBytes {
		err = fmt.Errorf("image exceeds %s", sizeText(maxSentFileBytes))
	}
	return data, filepath.Base(real), err
}

func (r *webRun) takeViewedImages() map[int]provider.Part {
	r.mu.Lock()
	defer r.mu.Unlock()
	images := r.viewImages
	r.viewImages = nil
	return images
}
