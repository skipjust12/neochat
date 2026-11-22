package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"neochat/attachment"
	"neochat/provider"
)

// Media bots return text/attachment content through Poe's non-streaming
// compatibility endpoints. The native Videos API is used only when advertised.
// Credentials are sent only to the configured API origin, never result URLs.
func (c *Client) poeCall(ctx context.Context, key, method, path string, body any, limit int64) ([]byte, error) {
	if strings.TrimSpace(key) == "" {
		return nil, ErrMissingKey
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = provider.PoeBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(key))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	// An authenticated content download can redirect to a signed CDN URL. Reject
	// redirects here; callers can fetch that URL using the separate safe downloader.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 && method == http.MethodGet && strings.HasSuffix(path, "/content") {
		target, err := response.Location()
		if err != nil {
			return nil, err
		}
		return c.download(ctx, target.String(), limit)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("Poe media response exceeds %d bytes", limit)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &provider.StatusError{StatusCode: response.StatusCode, Body: string(data)}
	}
	return data, nil
}
func poeMediaBody(model, prompt, endpoint string, images, videos []Reference, params map[string]any) map[string]any {
	parts := []any{}
	if endpoint == "/responses" {
		parts = append(parts, map[string]any{"type": "input_text", "text": prompt})
		for _, r := range images {
			parts = append(parts, map[string]any{"type": "input_image", "image_url": inlinePoe(r)})
		}
		for _, r := range videos {
			parts = append(parts, map[string]any{"type": "input_file", "filename": "reference.mp4", "file_data": inlinePoe(r)})
		}
	} else {
		parts = append(parts, map[string]any{"type": "text", "text": prompt})
		for _, r := range images {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": inlinePoe(r)}})
		}
		for _, r := range videos {
			parts = append(parts, map[string]any{"type": "file", "file": map[string]string{"filename": "reference.mp4", "file_data": inlinePoe(r)}})
		}
	}
	body := map[string]any{"model": model, "stream": false}
	if endpoint == "/responses" {
		body["input"] = []any{map[string]any{"role": "user", "content": parts}}
		body["store"] = false
	} else {
		body["messages"] = []any{map[string]any{"role": "user", "content": parts}}
	}
	for k, v := range params {
		if v != "" {
			body[k] = v
		}
	}
	return body
}
func inlinePoe(r Reference) string {
	return "data:" + r.MIME + ";base64," + base64.StdEncoding.EncodeToString(r.Data)
}

var poeMediaLinks = regexp.MustCompile(`!?\[[^\]]*\]\(<?(https?://[^\s)>]+)>?\)`)

func poeMediaOutput(data []byte) (results, string, error) {
	var wire struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Output []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
			Result  string          `json:"result"`
		} `json:"output"`
	}
	found := results{}
	if err := json.Unmarshal(data, &wire); err != nil {
		return found, "", err
	}
	if wire.Error != nil {
		return found, "", &FailedError{Message: wire.Error.Message}
	}
	if wire.Status != "" && wire.Status != "completed" {
		return found, "", fmt.Errorf("Poe media response status %q", wire.Status)
	}
	text := ""
	content := func(raw json.RawMessage) {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			text += s
			return
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) == nil {
			for _, p := range parts {
				text += p.Text
			}
		}
		found.collect(raw, 0)
	}
	for _, choice := range wire.Choices {
		content(choice.Message.Content)
	}
	for _, item := range wire.Output {
		content(item.Content)
		if item.Type == "image_generation_call" && item.Result != "" {
			found.add("b64_json", item.Result)
		}
	}
	for _, match := range poeMediaLinks.FindAllStringSubmatch(text, -1) {
		found.add("url", match[1])
	}
	// Remove only attachment links from the displayed text after their bytes are
	// stored locally; no provider storage URL needs to survive in chat history.
	text = poeMediaLinks.ReplaceAllString(text, "")
	return found, strings.TrimSpace(text), nil
}
func (c *Client) generatePoeImage(ctx context.Context, key string, req Request) (Result, error) {
	wait := c.MaxWait
	if wait <= 0 {
		wait = defaultMaxWait
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	endpoint := req.Endpoint
	if endpoint == "" {
		endpoint = "/chat/completions"
	}
	data, err := c.poeMediaQuery(ctx, key, req.Model, req.Prompt, endpoint, req.References, nil, map[string]any{"aspect_ratio": req.AspectRatio})
	if err != nil {
		return Result{}, err
	}
	found, text, err := poeMediaOutput(data)
	out := Result{Text: text}
	if err != nil {
		return out, err
	}
	for _, link := range found.urls {
		if len(found.inline) >= maxImages {
			break
		}
		data, err := c.download(ctx, link, c.maxImageBytes())
		if err != nil {
			return out, err
		}
		found.inline = append(found.inline, data)
	}
	for _, data := range found.inline {
		if len(out.Images) == maxImages {
			break
		}
		if int64(len(data)) > c.maxImageBytes() {
			return out, errors.New("Poe image too large")
		}
		image, err := decodeImage(data)
		if err != nil {
			return out, err
		}
		out.Images = append(out.Images, image)
	}
	if len(out.Images) == 0 && out.Text == "" {
		return out, errors.New("Poe returned no image attachment")
	}
	return out, nil
}
func (c *Client) generatePoeVideo(ctx context.Context, key string, req VideoRequest) (VideoResult, error) {
	wait := c.MaxVideoWait
	if wait <= 0 {
		wait = defaultVideoMaxWait
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	limit := c.MaxVideoBytes
	if limit <= 0 {
		limit = defaultMaxVideoBytes
	}
	if req.Endpoint == "/videos" && !c.BotQuery {
		return c.poeNativeVideo(ctx, key, req, limit)
	}
	endpoint := req.Endpoint
	if endpoint == "" {
		endpoint = "/chat/completions"
	}
	if req.VideoEnd > 0 && len(req.Videos) > 0 {
		ref, err := clipPoeReference(ctx, req.Videos[0], req.VideoEnd)
		if err != nil {
			return VideoResult{}, err
		}
		req.Videos = append([]Reference(nil), req.Videos...)
		req.Videos[0] = ref
	}
	params := map[string]any{"resolution": req.Resolution, "aspect_ratio": req.AspectRatio}
	if len(req.Videos) == 0 && req.Duration != "" && req.Duration != "auto" {
		params["duration"] = req.Duration
	}
	data, err := c.poeMediaQuery(ctx, key, req.Model, req.Prompt, endpoint, req.Images, req.Videos, params)
	if err != nil {
		return VideoResult{}, err
	}
	found, text, err := poeMediaOutput(data)
	out := VideoResult{Text: text}
	if err != nil {
		return out, err
	}
	for _, link := range found.urls {
		data, err := c.download(ctx, link, limit)
		if err != nil {
			return out, err
		}
		if mime := attachment.VideoMIME(data); mime != "" {
			out.Video = Video{MIME: mime, Data: data}
			return out, nil
		}
	}
	return out, errors.New("Poe returned no video attachment")
}
func (c *Client) poeNativeVideo(ctx context.Context, key string, req VideoRequest, limit int64) (VideoResult, error) {
	if len(req.Videos) > 0 || len(req.Images) > 1 {
		return VideoResult{}, errors.New("Poe Videos API accepts at most one reference image")
	}
	body := map[string]any{"model": req.Model, "prompt": req.Prompt}
	if req.Duration != "" {
		n, err := strconv.Atoi(req.Duration)
		if err != nil || n <= 0 {
			return VideoResult{}, errors.New("invalid video duration")
		}
		body["seconds"] = n
	}
	height := 720
	switch req.Resolution {
	case "1080p":
		height = 1080
	case "360p":
		height = 360
	case "4k":
		height = 2160
	}
	width := height * 16 / 9
	if req.AspectRatio == "9:16" {
		width, height = height, width
	}
	body["size"] = fmt.Sprintf("%dx%d", width, height)
	if len(req.Images) == 1 {
		body["input_image"] = base64.StdEncoding.EncodeToString(req.Images[0].Data)
	}
	data, err := c.poeCall(ctx, key, http.MethodPost, "/videos", body, maxStatusBytes)
	if err != nil {
		return VideoResult{}, err
	}
	var state struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	poll := c.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	id := ""
	for {
		if err = json.Unmarshal(data, &state); err != nil {
			return VideoResult{}, err
		}
		if state.ID != "" {
			if id != "" && id != state.ID {
				return VideoResult{}, errors.New("Poe changed video ID")
			}
			id = state.ID
		}
		if id == "" {
			return VideoResult{}, errors.New("Poe video has no ID")
		}
		switch state.Status {
		case "completed":
			data, err = c.poeCall(ctx, key, http.MethodGet, "/videos/"+url.PathEscape(id)+"/content", nil, limit)
			if err != nil {
				return VideoResult{}, err
			}
			mime := attachment.VideoMIME(data)
			if mime == "" {
				return VideoResult{}, errors.New("Poe video content is not a video")
			}
			return VideoResult{Video: Video{MIME: mime, Data: data}}, nil
		case "failed", "cancelled":
			message := "Video generation failed"
			if state.Error != nil {
				message = state.Error.Message
			}
			return VideoResult{}, &FailedError{Message: message}
		case "queued", "in_progress":
		default:
			return VideoResult{}, fmt.Errorf("Poe video status %q", state.Status)
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return VideoResult{}, &TimeoutError{ID: id}
			}
			return VideoResult{}, ctx.Err()
		case <-timer.C:
		}
		data, err = c.poeCall(ctx, key, http.MethodGet, "/videos/"+url.PathEscape(id), nil, maxStatusBytes)
		if err != nil {
			return VideoResult{}, err
		}
	}
}

func (c *Client) poeMediaQuery(ctx context.Context, key, model, prompt, endpoint string, images, videos []Reference, params map[string]any) ([]byte, error) {
	if !c.BotQuery {
		return c.poeCall(ctx, key, http.MethodPost, endpoint, poeMediaBody(model, prompt, endpoint, images, videos, params), 140<<20)
	}
	client := provider.NewPoeClient(key)
	client.BotQuery = true
	client.BotBaseURL = c.BotBaseURL
	client.UploadURL = c.UploadURL
	client.HTTP = c.HTTP
	parts := []provider.Part{{Type: provider.PartText, Text: prompt}}
	for i, ref := range images {
		parts = append(parts, provider.Part{Type: provider.PartImage, MIME: ref.MIME, Data: ref.Data, Name: fmt.Sprintf("reference-%d.png", i)})
	}
	for i, ref := range videos {
		parts = append(parts, provider.Part{Type: provider.PartVideo, MIME: ref.MIME, Data: ref.Data, Name: fmt.Sprintf("reference-%d.mp4", i)})
	}
	result, err := client.Generate(provider.WithPoeBotParameters(ctx, params), model, []provider.Message{{Role: "user", Content: prompt, Parts: parts}})
	if err != nil {
		return nil, err
	}
	content := []any{map[string]any{"type": "text", "text": result.Text}}
	for _, attachment := range result.Attachments {
		content = append(content, map[string]any{"type": "file", "url": attachment.URL})
	}
	return json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
}
