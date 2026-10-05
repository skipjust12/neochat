package imagegen

import (
	"context"
	"errors"
	"strings"
	"time"

	"neochat/attachment"
)

const (
	defaultVideoMaxWait  = 10 * time.Minute
	defaultMaxVideoBytes = 200 << 20
)

// VideoRequest is one video generation.
type VideoRequest struct {
	Model       string
	Prompt      string
	Duration    string // seconds, e.g. "6"; the model picks it when editing a video
	Resolution  string // e.g. "720p"
	AspectRatio string // e.g. "16:9"
	Images      []Reference
	// Videos are clips to edit or continue. VideoEnd, when set, takes the
	// first VideoEnd seconds of the first one (models take up to 10 s).
	Videos   []Reference
	VideoEnd float64
	User     string
}

// Video is a generated clip, downloaded.
type Video struct {
	MIME string
	Data []byte
}

// VideoResult is what a finished video generation produced.
type VideoResult struct {
	Video Video
	// Text is what the model said along with the clip, if anything.
	Text    string
	CostRUB float64
	HasCost bool
}

// GenerateVideo runs one video generation to the end and downloads the
// clip. Generations are asynchronous on Polza's side ("async": true) and
// take minutes, so it polls for up to MaxVideoWait.
func (c *Client) GenerateVideo(ctx context.Context, apiKey string, req VideoRequest) (VideoResult, error) {
	if strings.TrimSpace(apiKey) == "" {
		return VideoResult{}, ErrMissingKey
	}
	input := map[string]any{"prompt": req.Prompt}
	if req.AspectRatio != "" {
		input["aspect_ratio"] = req.AspectRatio
	}
	if req.Resolution != "" {
		input["resolution"] = req.Resolution
	}
	if len(req.Videos) == 0 && req.Duration != "" {
		input["duration"] = req.Duration
	}
	if len(req.Images) > 0 {
		input["images"] = inlineFiles(req.Images)
	}
	if len(req.Videos) > 0 {
		input["videos"] = inlineFiles(req.Videos)
		if req.VideoEnd > 0 {
			input["video_start"] = 0
			input["video_end"] = req.VideoEnd
		}
	}
	body := map[string]any{"model": req.Model, "input": input, "async": true}
	if req.User != "" {
		body["user"] = req.User
	}
	maxWait := c.MaxVideoWait
	if maxWait <= 0 {
		maxWait = defaultVideoMaxWait
	}
	status, err := c.run(ctx, apiKey, body, maxWait)
	if err != nil {
		return VideoResult{}, err
	}
	result := VideoResult{Text: strings.TrimSpace(status.Content)}
	result.CostRUB, result.HasCost = status.cost()

	var found results
	found.collect(status.Data, 0)
	found.collect(status.Output, 0)
	limit := c.MaxVideoBytes
	if limit <= 0 {
		limit = defaultMaxVideoBytes
	}
	// The result may point at a cover picture as well as the clip: the
	// first link that turns out to be a video is the one.
	for _, link := range found.urls {
		data, err := c.download(ctx, link, limit)
		if err != nil {
			return result, err
		}
		if mime := attachment.VideoMIME(data); mime != "" {
			result.Video = Video{MIME: mime, Data: data}
			return result, nil
		}
	}
	return result, errors.New("imagegen: the generation finished without a video")
}
