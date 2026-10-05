package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"neochat/attachment"
	"neochat/conversation"
	"neochat/imagegen"
	"neochat/limits"
	"neochat/provider"
	"neochat/router"
)

// Video answers. A model of kind router.KindVideo (Gemini Omni) doesn't
// chat: the user's message becomes the prompt for Polza's Media API, run on
// the user's image key like an image model (image.go), and the answer is
// the clip it makes. The composer sends the length, resolution and shape
// (chatRequest.Video). Pictures attached to the message guide the clip; an
// attached video is edited or continued -- the model then picks the length,
// and only the first 10 seconds of a longer one are used. Unlike pictures,
// a follow-up message doesn't reuse the chat's last clip by itself: editing
// one costs more than making one, so the user picks it ("Use as
// reference" in the chat). Clips are kept as the chat's files.

// videoChoice is what the composer asks of a video model.
type videoChoice struct {
	Duration    string `json:"duration,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
}

// maxReferenceVideoSeconds is how much of an attached video the model
// takes.
const maxReferenceVideoSeconds = 10

// videoNote is how an answer that made a clip reads to a chat model in
// later turns.
const videoNote = "[You made a video for this request; the user sees it in the chat.]"

var errMissingVideoKey = userError{text: "Add your Image API key in Settings → Account to make videos."}

// videoAnswer makes the clip a message asks for.
func (s *Server) videoAnswer(ctx context.Context, req chatRequest, prepared preparedRequest, result router.RouteResult, model router.Model) (webAnswer, error) {
	if s.Images == nil || s.Attachments == nil || model.Video == nil {
		return webAnswer{}, userError{text: "Video generation isn't available on this server."}
	}
	if strings.TrimSpace(req.imageKey) == "" {
		return webAnswer{}, errMissingVideoKey
	}
	prompt := strings.TrimSpace(withQuote(req.Quote, req.Message))
	if prompt == "" {
		return webAnswer{}, userError{text: "Describe the video you want."}
	}
	choice, err := videoChoiceFor(model, req.Video)
	if err != nil {
		return webAnswer{}, err
	}
	images, videos, err := s.videoReferences(ctx, req, prepared, model)
	if err != nil {
		return webAnswer{}, err
	}
	price, ok := model.Video.PriceRUB(map[string]string{"duration": choice.Duration, "resolution": choice.Resolution, "has_video": fmt.Sprint(len(videos) > 0)})
	if !ok {
		return webAnswer{}, fmt.Errorf("video model %q has no price for %+v", model.ID, choice)
	}
	amount := price / rubPerUSD

	pool := generationPool(result, model)
	cap := prepared.plan.ThinkingMaxCapUSD
	if pool == limits.PoolInstant {
		cap = prepared.plan.InstantExtraCapUSD
	}
	reservation, err := s.Store.Reserve(ctx, req.UserID, pool, amount, cap)
	if err != nil {
		return webAnswer{}, err
	}
	request := imagegen.VideoRequest{
		Model: model.ResolveAPIModelID(), Prompt: prompt, Duration: choice.Duration, Resolution: choice.Resolution, AspectRatio: choice.AspectRatio,
		Images: images, Videos: videos, User: req.UserID,
	}
	if len(videos) > 0 {
		if seconds, ok := attachment.VideoDuration(videos[0].Data); ok && seconds > maxReferenceVideoSeconds {
			request.VideoEnd = maxReferenceVideoSeconds
		}
	}
	made, genErr := s.Images.GenerateVideo(ctx, req.imageKey, request)

	// Settled like a picture (imageAnswer): a refusal or a failed
	// generation costs nothing, a finished one what Polza says (or the
	// tier price), a stop or a timeout the tier price, since Polza goes on
	// making it; anything else keeps the reservation for reconciliation.
	var statusErr *provider.StatusError
	var failed *imagegen.FailedError
	var timedOut *imagegen.TimeoutError
	refused := errors.As(genErr, &failed) || (errors.As(genErr, &statusErr) && statusErr.StatusCode >= 400 && statusErr.StatusCode < 500 && statusErr.StatusCode != http.StatusRequestTimeout)
	actual := amount
	switch {
	case genErr == nil:
		if made.HasCost {
			actual = made.CostRUB / rubPerUSD
		}
	case refused:
		actual = 0
	case errors.Is(genErr, context.Canceled) || errors.As(genErr, &timedOut):
	default:
		log.Printf("server: retained reservation %s user_id=%s model=%s for an uncertain video generation: %v", reservation.ID, req.UserID, model.ID, genErr)
		return webAnswer{}, mediaError(genErr, "video")
	}
	billCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.Store.Settle(billCtx, reservation, actual); err != nil {
		return webAnswer{}, fmt.Errorf("settle spend: %w", err)
	}
	if genErr != nil {
		return webAnswer{}, mediaError(genErr, "video")
	}

	ref, err := s.storeVideo(ctx, req.UserID, made.Video, choice)
	if err != nil {
		return webAnswer{}, err
	}
	return webAnswer{GenerateResult: provider.GenerateResult{Text: made.Text}, Videos: []conversation.Attachment{ref}, MediaCostUSD: actual}, nil
}

// videoChoiceFor checks the composer's choices against what the model
// offers; an empty one takes the default: the shortest length, 720p when
// offered, the first shape.
func videoChoiceFor(model router.Model, asked *videoChoice) (videoChoice, error) {
	options := model.Video
	choice := videoChoice{Duration: options.Durations[0], Resolution: options.Resolutions[0], AspectRatio: options.AspectRatios[0]}
	if slices.Contains(options.Resolutions, "720p") {
		choice.Resolution = "720p"
	}
	if asked == nil {
		return choice, nil
	}
	for _, field := range []struct {
		value   string
		allowed []string
		set     *string
		what    string
	}{
		{asked.Duration, options.Durations, &choice.Duration, "length"},
		{asked.Resolution, options.Resolutions, &choice.Resolution, "resolution"},
		{asked.AspectRatio, options.AspectRatios, &choice.AspectRatio, "shape"},
	} {
		if field.value == "" {
			continue
		}
		if !slices.Contains(field.allowed, field.value) {
			return videoChoice{}, userErrorf("%s doesn't offer %s %s. Pick one of: %s.", modelName(model), field.what, clipRunes(field.value, 20), strings.Join(field.allowed, ", "))
		}
		*field.set = field.value
	}
	return choice, nil
}

// videoReferences loads what the model works from: the message's pictures
// and videos. A video counts as two pictures against the model's limit.
func (s *Server) videoReferences(ctx context.Context, req chatRequest, prepared preparedRequest, model router.Model) (images, videos []imagegen.Reference, err error) {
	var imageIDs, videoIDs []string
	for _, ref := range prepared.attachments {
		switch ref.Kind {
		case attachment.KindImage:
			imageIDs = append(imageIDs, ref.ID)
		case attachment.KindVideo:
			videoIDs = append(videoIDs, ref.ID)
		default:
			return nil, nil, userErrorf("%s works from pictures and videos only. Remove %s, or pick a chat model for it.", modelName(model), ref.Name)
		}
	}
	if len(videoIDs) > model.Video.MaxReferenceVideos {
		if model.Video.MaxReferenceVideos == 0 {
			return nil, nil, userErrorf("%s can't work from a video. Remove it, or pick another model.", modelName(model))
		}
		return nil, nil, userErrorf("%s takes one video at a time.", modelName(model))
	}
	if len(imageIDs)+2*len(videoIDs) > model.MaxReferenceImages {
		return nil, nil, userErrorf("%s takes at most %d pictures at a time, and a video counts as two.", modelName(model), model.MaxReferenceImages)
	}
	load := func(ids []string) ([]imagegen.Reference, error) {
		out := make([]imagegen.Reference, 0, len(ids))
		for _, id := range ids {
			file, err := s.Attachments.Get(ctx, req.UserID, id)
			if err != nil {
				return nil, fmt.Errorf("load reference %s: %w", id, err)
			}
			out = append(out, imagegen.Reference{MIME: file.MIME, Data: file.Data})
		}
		return out, nil
	}
	if images, err = load(imageIDs); err != nil {
		return nil, nil, err
	}
	if videos, err = load(videoIDs); err != nil {
		return nil, nil, err
	}
	return images, videos, nil
}

// storeVideo keeps a generated clip as an unclaimed file; persistTurn
// claims it for the chat once the turn is stored. Its size and length are
// what was asked for, unless the file says its length.
func (s *Server) storeVideo(ctx context.Context, userID string, video imagegen.Video, choice videoChoice) (conversation.Attachment, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	id := conversation.NewID()
	file := attachment.File{
		ID: id, UserID: userID, Name: "video-" + id[:8] + videoExtension(video.MIME), MIME: video.MIME,
		Kind: attachment.KindVideo, Size: int64(len(video.Data)), Data: video.Data, CreatedAt: time.Now(),
	}
	if err := s.Attachments.Put(ctx, file); err != nil {
		return conversation.Attachment{}, fmt.Errorf("store generated video: %w", err)
	}
	width, height := videoSize(choice.Resolution, choice.AspectRatio)
	seconds, ok := attachment.VideoDuration(video.Data)
	if !ok {
		fmt.Sscan(choice.Duration, &seconds)
	}
	return conversation.Attachment{ID: file.ID, Name: file.Name, MIME: file.MIME, Kind: file.Kind, Size: file.Size, Width: width, Height: height, Seconds: seconds}, nil
}

func videoExtension(mime string) string {
	switch mime {
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	}
	return ".mp4"
}

// videoSize is the frame size a resolution and shape name ("720p", "9:16"
// is 720x1280).
func videoSize(resolution, aspectRatio string) (width, height int) {
	short := map[string]int{"360p": 360, "480p": 480, "720p": 720, "1080p": 1080, "4k": 2160, "2160p": 2160}[resolution]
	if short == 0 {
		short = 720
	}
	long := short * 16 / 9
	if aspectRatio == "9:16" {
		return short, long
	}
	return long, short
}
