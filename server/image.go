package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"neochat/attachment"
	"neochat/conversation"
	"neochat/imagegen"
	"neochat/limits"
	"neochat/provider"
	"neochat/router"
)

// Image answers. A model of kind router.KindImage doesn't chat: the user's
// message becomes the prompt for Polza's Media API (package imagegen), run
// on the user's separate image key (X-Image-Key, Settings -> Account,
// never stored), and the answer is the picture it makes. Pictures are
// kept as the chat's files (package attachment), claimed by the chat once
// the turn is stored, so deleting the chat deletes them, and an incognito
// chat's pictures go with the hourly sweep of unclaimed files.
//
// What the model is given to work from: the pictures attached to the
// message, or else the latest picture this chat made -- so "make the sky
// darker" edits the last image instead of starting over. A new chat starts
// from scratch.

// imageKeyHeader carries the user's image key, like providerKeyHeader the
// chat key.
const imageKeyHeader = "X-Image-Key"

// rubPerUSD is the rate the catalog's prices were converted from Polza's
// rubles at (docs/running-locally.md), used here the other way: Polza
// reports what an image cost in rubles.
const rubPerUSD = 117.068

var errMissingImageKey = userError{text: "Add your Image API key in Settings → Account to make images."}

// imageAnswer makes the picture a message asks for.
func (s *Server) imageAnswer(ctx context.Context, req chatRequest, prepared preparedRequest, result router.RouteResult, model router.Model) (webAnswer, error) {
	if s.Images == nil || s.Attachments == nil {
		return webAnswer{}, userError{text: "Image generation isn't available on this server."}
	}
	if strings.TrimSpace(req.imageKey) == "" {
		return webAnswer{}, errMissingImageKey
	}
	prompt := strings.TrimSpace(withQuote(req.Quote, req.Message))
	if prompt == "" {
		return webAnswer{}, userError{text: "Describe the image you want."}
	}
	references, err := s.imageReferences(ctx, req, prepared, model)
	if err != nil {
		return webAnswer{}, err
	}

	amount := model.CostPerImageUSD
	pool := generationPool(result, model)
	cap := prepared.plan.ThinkingMaxCapUSD
	if pool == limits.PoolInstant {
		cap = prepared.plan.InstantExtraCapUSD
	}
	reservation, err := s.Store.Reserve(ctx, req.UserID, pool, amount, cap)
	if err != nil {
		return webAnswer{}, err
	}
	made, genErr := s.Images.Generate(ctx, req.imageKey, imagegen.Request{Model: model.ResolveAPIModelID(), Prompt: prompt, References: references})

	// Settle like billedCall: a refusal or a generation Polza reports as
	// failed costs nothing; a finished one costs what Polza says (or the
	// catalog price); a stop or a timeout keeps the picture's price, since
	// Polza goes on making it; anything else keeps the reservation for
	// reconciliation.
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
		log.Printf("server: retained reservation %s user_id=%s model=%s for an uncertain image generation: %v", reservation.ID, req.UserID, model.ID, genErr)
		return webAnswer{}, imageError(genErr)
	}
	billCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.Store.Settle(billCtx, reservation, actual); err != nil {
		return webAnswer{}, fmt.Errorf("settle spend: %w", err)
	}
	if genErr != nil {
		return webAnswer{}, imageError(genErr)
	}

	refs, err := s.storeImages(ctx, req.UserID, made.Images)
	if err != nil {
		return webAnswer{}, err
	}
	return webAnswer{GenerateResult: provider.GenerateResult{Text: made.Text}, Images: refs, ImageCostUSD: actual}, nil
}

// imageReferences loads what the model works from: the message's own
// pictures, or else the chat's latest generated one.
func (s *Server) imageReferences(ctx context.Context, req chatRequest, prepared preparedRequest, model router.Model) ([]imagegen.Reference, error) {
	ids := make([]string, 0, len(prepared.attachments))
	for _, ref := range prepared.attachments {
		if ref.Kind != attachment.KindImage {
			return nil, userErrorf("%s works from pictures only. Remove %s, or pick a chat model for it.", modelName(model), ref.Name)
		}
		ids = append(ids, ref.ID)
	}
	if len(ids) == 0 && prepared.lastImage != nil {
		ids = append(ids, prepared.lastImage.ID)
	}
	if len(ids) > model.MaxReferenceImages {
		if len(prepared.attachments) > model.MaxReferenceImages {
			return nil, userErrorf("%s takes at most %d pictures at a time.", modelName(model), model.MaxReferenceImages)
		}
		ids = ids[:model.MaxReferenceImages]
	}
	references := make([]imagegen.Reference, 0, len(ids))
	for _, id := range ids {
		file, err := s.Attachments.Get(ctx, req.UserID, id)
		if errors.Is(err, attachment.ErrNotFound) && len(prepared.attachments) == 0 {
			continue // the last picture is gone: start from the prompt alone
		}
		if err != nil {
			return nil, fmt.Errorf("load reference image %s: %w", id, err)
		}
		references = append(references, imagegen.Reference{MIME: file.MIME, Data: file.Data})
	}
	return references, nil
}

// storeImages keeps generated pictures as unclaimed files; persistTurn
// claims them for the chat once the turn is stored.
func (s *Server) storeImages(ctx context.Context, userID string, images []imagegen.Image) ([]conversation.Attachment, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	now := time.Now()
	refs := make([]conversation.Attachment, 0, len(images))
	for _, picture := range images {
		id := conversation.NewID()
		file := attachment.File{
			ID: id, UserID: userID, Name: "image-" + id[:8] + imageExtension(picture.MIME), MIME: picture.MIME,
			Kind: attachment.KindImage, Size: int64(len(picture.Data)), Data: picture.Data, CreatedAt: now,
		}
		if err := s.Attachments.Put(ctx, file); err != nil {
			return nil, fmt.Errorf("store generated image: %w", err)
		}
		refs = append(refs, conversation.Attachment{ID: file.ID, Name: file.Name, MIME: file.MIME, Kind: file.Kind, Size: file.Size, Width: picture.Width, Height: picture.Height})
	}
	return refs, nil
}

func imageExtension(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	return ".png"
}

// imageError turns a failed generation into what the user is told. The
// key, balance and limit cases name the image key, since that's the one
// to check; Polza's own explanation of a failed generation (a content
// policy refusal, say) is shown as is.
func imageError(err error) error {
	var statusErr *provider.StatusError
	var failed *imagegen.FailedError
	var timedOut *imagegen.TimeoutError
	switch {
	case errors.Is(err, imagegen.ErrMissingKey):
		return errMissingImageKey
	case errors.As(err, &failed):
		if failed.Message == "" {
			return userError{text: "The image couldn't be made. Try rephrasing the request."}
		}
		return userErrorf("The image couldn't be made: %s", clipRunes(failed.Message, 300))
	case errors.As(err, &timedOut):
		return userError{text: "The image is taking too long. Try again in a moment."}
	case errors.As(err, &statusErr) && statusErr.StatusCode < 500:
		switch statusErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return userError{text: "Polza AI rejected your Image API key. Check it in Settings → Account."}
		case http.StatusPaymentRequired:
			return userError{text: "The balance behind your Image API key is too low for this image. Top it up on polza.ai."}
		case http.StatusTooManyRequests:
			return userError{text: "Polza AI is rate limiting your Image API key. Wait a moment and try again."}
		}
		if detail := provider.VendorMessage(statusErr.Body); detail != "" {
			return userErrorf("Polza AI: %s (HTTP %d)", clipRunes(detail, 300), statusErr.StatusCode)
		}
	}
	return err
}

// lastGeneratedImage is the newest picture an earlier answer in history
// made: what a follow-up message edits.
func lastGeneratedImage(history []conversation.Message) *conversation.Attachment {
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role != conversation.RoleAssistant || len(m.Versions) == 0 {
			continue
		}
		if images := m.Versions[len(m.Versions)-1].Images; len(images) > 0 {
			last := images[len(images)-1]
			return &last
		}
	}
	return nil
}

// imageNote is how an answer that made pictures reads to a chat model in
// later turns: it can't see them, but it knows they're there.
func imageNote(images []conversation.Attachment) string {
	if len(images) == 1 {
		return "[You made an image for this request; the user sees it in the chat.]"
	}
	return fmt.Sprintf("[You made %d images for this request; the user sees them in the chat.]", len(images))
}
