package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"regexp"
	"sort"
	"strings"

	"neochat/attachment"
	"neochat/conversation"
	"neochat/provider"
)

// The marker is stored in the response text, so its position survives
// history, versions, replay and stopping a reply. It contains metadata only;
// the bytes still use the authenticated, owner-scoped /files endpoint.
type inlineImage struct {
	DisplayID string                  `json:"display_id"`
	File      conversation.Attachment `json:"file"`
	Caption   string                  `json:"caption,omitempty"`
}

type inlineImageDeltaKey struct{}

var showImageTool = provider.FunctionTool{
	Name:        "show_image",
	Description: "Display an existing picture from the terminal inline at this point in your answer, then continue writing. First create the PNG/JPEG/WebP/GIF with run_script or run_command. Use for diagrams, plots, charts and other programmatically drawn visuals that belong between paragraphs. This does not generate images and does not end your answer. Do not repeat the image in Markdown or send_file afterwards.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "Path to the picture on the selected terminal, relative to the chat's working directory or absolute."},
			"caption": map[string]any{"type": "string", "description": "Short caption and accessible description in the user's language."},
		},
		"required": []string{"path", "caption"},
	},
}

func validateInlineImage(data []byte) (image.Config, error) {
	kind, _, err := attachment.Classify("image", data)
	if err != nil || kind != attachment.KindImage {
		return image.Config{}, fmt.Errorf("show_image needs a PNG, JPEG, WebP or GIF picture; create one with run_script first")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return image.Config{}, fmt.Errorf("the image cannot be decoded: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > attachment.MaxImagePixels/cfg.Height {
		return image.Config{}, fmt.Errorf("the image exceeds the %d megapixel limit", attachment.MaxImagePixels/1_000_000)
	}
	return cfg, nil
}

func (r *webRun) planShowImage(call provider.ToolCall, position int) (string, func(context.Context) string) {
	if !r.terminalAllowed() || r.s.Attachments == nil {
		return "Error: showing images is not available.", nil
	}
	var args struct {
		Path    string `json:"path"`
		Caption string `json:"caption"`
	}
	if json.Unmarshal([]byte(call.Arguments), &args) != nil || strings.TrimSpace(args.Path) == "" {
		return "Error: path is required.", nil
	}
	caption := clipRunes(strings.Join(strings.Fields(args.Caption), " "), 500)
	index := r.start(conversation.ToolActivity{Tool: "show_image", Command: clipRunes(strings.TrimSpace(args.Path), 1000)})
	return "", func(ctx context.Context) string {
		file, err := r.sendTerminalFile(ctx, args.Path, "", true)
		r.finish(index, func(a *conversation.ToolActivity) {
			if err != nil {
				a.Error = err.Error()
				return
			}
			a.Title, a.Size = file.Name, file.Size
		})
		if err != nil {
			return "Error: " + err.Error()
		}
		r.mu.Lock()
		if r.inlineImages == nil {
			r.inlineImages = map[int]inlineImage{}
		}
		r.inlineImages[position] = inlineImage{DisplayID: conversation.NewID(), File: file, Caption: caption}
		r.mu.Unlock()
		return fmt.Sprintf("Displayed %s inline after your preceding text. Continue your explanation after the image; do not repeat it, link to it or call send_file for it.", file.Name)
	}
}

func (r *webRun) takeInlineImages() []inlineImage {
	r.mu.Lock()
	defer r.mu.Unlock()
	positions := make([]int, 0, len(r.inlineImages))
	for position := range r.inlineImages {
		positions = append(positions, position)
	}
	sort.Ints(positions)
	shown := make([]inlineImage, 0, len(positions))
	for _, position := range positions {
		shown = append(shown, r.inlineImages[position])
	}
	r.inlineImages = nil
	return shown
}

func inlineImageMarker(shown inlineImage) string {
	data, _ := json.Marshal(shown)
	return "\n\n<!--neochat-image:" + base64.RawURLEncoding.EncodeToString(data) + "-->\n\n"
}

var inlineImagePattern = regexp.MustCompile(`<!--neochat-image:([A-Za-z0-9_-]+)-->`)

func inlineImageFiles(content string) []conversation.Attachment {
	var files []conversation.Attachment
	seen := map[string]bool{}
	for _, match := range inlineImagePattern.FindAllStringSubmatch(content, -1) {
		data, err := base64.RawURLEncoding.DecodeString(match[1])
		if err != nil {
			continue
		}
		var shown inlineImage
		if json.Unmarshal(data, &shown) != nil || shown.File.ID == "" || shown.File.Kind != attachment.KindImage || seen[shown.File.ID] {
			continue
		}
		seen[shown.File.ID] = true
		files = append(files, shown.File)
	}
	return files
}

// Models and summaries get a readable note, never the internal display payload.
func inlineImageHistoryText(content string) string {
	return inlineImagePattern.ReplaceAllStringFunc(content, func(marker string) string {
		match := inlineImagePattern.FindStringSubmatch(marker)
		data, err := base64.RawURLEncoding.DecodeString(match[1])
		if err != nil {
			return ""
		}
		var shown inlineImage
		if json.Unmarshal(data, &shown) != nil {
			return ""
		}
		return "[Inline image shown: " + orDefault(shown.Caption, shown.File.Name) + "]"
	})
}
