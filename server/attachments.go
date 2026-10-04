package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"

	"neochat/attachment"
	"neochat/auth"
	"neochat/conversation"
	"neochat/pagestore"
	"neochat/provider"
	"neochat/router"
)

// userError is a failure whose text is written for the person chatting
// (wrong file for the chosen model, too many files...). clientErrorMessage
// passes it through verbatim and the HTTP status is 400.
type userError struct{ text string }

func (e userError) Error() string { return e.text }

func userErrorf(format string, args ...any) error {
	return userError{text: fmt.Sprintf(format, args...)}
}

// Byte budgets for what one request sends to the vendor. The current
// message may carry up to maxMessageAttachmentBytes; older messages' files
// are re-sent (the model needs them to keep talking about them) newest
// first until maxHistoryAttachmentBytes, after which they are replaced by a
// one-line note.
const (
	maxMessageAttachmentBytes = 30 << 20
	maxHistoryAttachmentBytes = 30 << 20
	maxUploadRequestBytes     = attachment.MaxUploadBytes + 64<<10 // multipart framing
	uploadReadTimeout         = 3 * time.Minute
)

type uploadResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	MIME string `json:"mime"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}

// handleFileUpload stores one file from a multipart/form-data body (field
// "file") and returns its id; the chat request then references it.
func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if s.Attachments == nil {
		http.Error(w, "file uploads are not configured on this server", http.StatusServiceUnavailable)
		return
	}
	// The server-wide ReadTimeout (30s) is sized for chat requests; a 20 MB
	// upload on a slow connection needs longer.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadTimeout))
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadRequestBytes)

	name, data, err := readUploadedFile(r)
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) || errors.Is(err, attachment.ErrTooLarge) {
			http.Error(w, fmt.Sprintf("file is too large (limit %d MB)", attachment.MaxUploadBytes>>20), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "send the file as multipart/form-data in a field named \"file\"", http.StatusBadRequest)
		return
	}
	name = attachment.SanitizeName(name)
	kind, mimeType, err := attachment.Classify(name, data)
	if err == nil && kind == attachment.KindImage {
		// Stored at the size models actually use; see NormalizeImage.
		data, mimeType, err = attachment.NormalizeImage(data, mimeType)
		name = attachment.RenameForMIME(name, mimeType)
	}
	if err != nil {
		status := http.StatusUnsupportedMediaType
		if errors.Is(err, attachment.ErrTooLarge) || errors.Is(err, attachment.ErrImageDimensions) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}

	ctx := r.Context()
	if err := s.Attachments.PruneUnclaimed(ctx, identity.UserID, time.Now().Add(-attachment.UnclaimedTTL)); err != nil {
		log.Printf("server: prune unclaimed attachments for user_id=%s: %v", identity.UserID, err)
	}
	pending, err := s.Attachments.CountUnclaimed(ctx, identity.UserID)
	if err != nil {
		log.Printf("server: count unclaimed attachments for user_id=%s: %v", identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	if pending >= attachment.MaxUnclaimedPerUser {
		http.Error(w, "too many files waiting to be sent; send or remove some first", http.StatusTooManyRequests)
		return
	}

	file := attachment.File{
		ID: conversation.NewID(), UserID: identity.UserID, Name: name, MIME: mimeType, Kind: kind,
		Size: int64(len(data)), Data: data, CreatedAt: time.Now(),
	}
	if err := s.Attachments.Put(ctx, file); err != nil {
		log.Printf("server: store attachment for user_id=%s: %v", identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(uploadResponse{ID: file.ID, Name: file.Name, MIME: file.MIME, Kind: file.Kind, Size: file.Size}); err != nil {
		log.Printf("server: encode upload response: %v", err)
	}
}

// readUploadedFile streams the "file" part, reading at most one byte more
// than the largest accepted file so an oversized upload fails without
// being buffered whole.
func readUploadedFile(r *http.Request) (string, []byte, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return "", nil, err
	}
	for {
		part, err := reader.NextPart()
		if err != nil {
			return "", nil, err
		}
		if part.FormName() != "file" {
			part.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, attachment.MaxUploadBytes+1))
		part.Close()
		if err != nil {
			return "", nil, err
		}
		if len(data) > attachment.MaxUploadBytes {
			return "", nil, attachment.ErrTooLarge
		}
		if len(data) == 0 {
			return "", nil, errors.New("empty file")
		}
		return part.FileName(), data, nil
	}
}

// handleFileDownload returns one of the caller's own files. Only the four
// raster image types are served inline (for thumbnails); everything else
// is a forced download with a sandbox CSP, so an uploaded HTML or SVG file
// can never run script on this origin.
func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if s.Attachments == nil {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(r.PathValue("file_id"))
	if id == "" || len(id) > 64 {
		http.NotFound(w, r)
		return
	}
	file, err := s.Attachments.Get(r.Context(), identity.UserID, id)
	if errors.Is(err, attachment.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("server: load attachment %s for user_id=%s: %v", id, identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	disposition := "attachment"
	contentType := "application/octet-stream"
	if file.Kind == attachment.KindImage {
		disposition, contentType = "inline", file.MIME
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": file.Name}))
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// No browser caching (recovered already sends no-store): a file must
	// stop being reachable the moment its chat is deleted. The page keeps
	// its own in-memory copy for thumbnails, so this costs one fetch per
	// file per page load.
	_, _ = w.Write(file.Data)
}

// loadRequestAttachments resolves the ids a chat request carries into the
// caller's files, enforcing ownership, the per-message limits, and that a
// file already sent in one conversation isn't reused in another.
func (s *Server) loadRequestAttachments(ctx context.Context, req chatRequest, conversationID string) ([]attachment.File, error) {
	if len(req.Attachments) == 0 {
		return nil, nil
	}
	if s.Attachments == nil {
		return nil, userErrorf("File uploads are not available on this server.")
	}
	files := make([]attachment.File, 0, len(req.Attachments))
	total := int64(0)
	for _, id := range req.Attachments {
		file, err := s.Attachments.Get(ctx, req.UserID, id)
		if errors.Is(err, attachment.ErrNotFound) {
			return nil, userErrorf("One of the attached files is no longer available. Remove it and attach it again.")
		}
		if err != nil {
			return nil, fmt.Errorf("load attachment: %w", err)
		}
		if file.ConversationID != "" && file.ConversationID != conversationID {
			return nil, userErrorf("%s belongs to another chat. Attach it again here.", file.Name)
		}
		total += file.Size
		files = append(files, file)
	}
	if total > maxMessageAttachmentBytes {
		return nil, userErrorf("Files in one message are limited to %d MB in total.", maxMessageAttachmentBytes>>20)
	}
	return files, nil
}

// userMessage builds the provider message for a user turn: files first
// (vendors recommend images before the question), then the typed text.
// Text files are inlined so any model can read them; notes stand in for
// files that can't be sent. Content mirrors every text part, which keeps
// token estimates and spend bounds honest about inlined files.
func userMessage(text string, files []attachment.File, notes []string) provider.Message {
	if len(files) == 0 && len(notes) == 0 {
		return provider.Message{Role: "user", Content: text}
	}
	parts := make([]provider.Part, 0, len(files)+len(notes)+1)
	var texts []string
	addText := func(t string) {
		parts = append(parts, provider.Part{Type: provider.PartText, Text: t})
		texts = append(texts, t)
	}
	for _, f := range files {
		switch f.Kind {
		case attachment.KindImage:
			parts = append(parts, provider.Part{Type: provider.PartImage, MIME: f.MIME, Name: f.Name, Data: f.Data})
		case attachment.KindDocument:
			parts = append(parts, provider.Part{Type: provider.PartFile, MIME: f.MIME, Name: f.Name, Data: f.Data})
		default:
			addText(fmt.Sprintf("<file name=%q>\n%s\n</file>", f.Name, attachment.Text(f)))
		}
	}
	for _, note := range notes {
		addText(note)
	}
	if strings.TrimSpace(text) != "" {
		addText(text)
	}
	return provider.Message{Role: "user", Content: strings.Join(texts, "\n\n"), Parts: parts}
}

// historyMessage rebuilds an earlier turn, re-attaching its files while
// the shared history byte budget lasts. Files that are gone or over budget
// become a short note so the model still knows something was attached.
func (s *Server) historyMessage(ctx context.Context, userID string, m conversation.Message, budget *int64) provider.Message {
	if m.Role == conversation.RoleAssistant {
		return provider.Message{Role: string(m.Role), Content: assistantHistoryContent(m)}
	}
	if m.Role != conversation.RoleUser || len(m.Attachments) == 0 {
		return provider.Message{Role: string(m.Role), Content: m.Content}
	}
	var files []attachment.File
	var notes []string
	for _, ref := range m.Attachments {
		if s.Attachments == nil || ref.Size > *budget {
			notes = append(notes, fmt.Sprintf("[Earlier attachment %q is not included in this request.]", ref.Name))
			continue
		}
		file, err := s.Attachments.Get(ctx, userID, ref.ID)
		if err != nil {
			if !errors.Is(err, attachment.ErrNotFound) {
				log.Printf("server: load history attachment %s for user_id=%s: %v", ref.ID, userID, err)
			}
			notes = append(notes, fmt.Sprintf("[Earlier attachment %q is no longer available.]", ref.Name))
			continue
		}
		*budget -= file.Size
		files = append(files, file)
	}
	return userMessage(m.Content, files, notes)
}

func attachmentRefs(files []attachment.File) []conversation.Attachment {
	if len(files) == 0 {
		return nil
	}
	refs := make([]conversation.Attachment, len(files))
	for i, f := range files {
		refs[i] = conversation.Attachment{ID: f.ID, Name: f.Name, MIME: f.MIME, Kind: f.Kind, Size: f.Size}
	}
	return refs
}

// adaptForModel fits the message list to what model can read. Files in the
// message being sent now must be readable -- silently dropping them would
// answer a question the user didn't ask -- so that is an error naming the
// model. Files from earlier turns degrade to a note instead, so switching
// to a text-only model mid-chat still works.
func adaptForModel(messages []provider.Message, model router.Model) ([]provider.Message, error) {
	images, files := modelAccepts(model, "image"), modelAccepts(model, "file")
	last := len(messages) - 1
	var out []provider.Message
	for i, m := range messages {
		if len(m.Parts) == 0 {
			continue
		}
		changed := false
		parts := make([]provider.Part, 0, len(m.Parts))
		for _, p := range m.Parts {
			unsupported := (p.Type == provider.PartImage && !images) || (p.Type == provider.PartFile && !files)
			if !unsupported {
				parts = append(parts, p)
				continue
			}
			if i == last {
				what := "images"
				if p.Type == provider.PartFile {
					what = "PDF or DOCX files"
				}
				return nil, userErrorf("%s can't read %s. Pick a model that can (Claude, GPT, Gemini or Grok), or remove %s.", modelName(model), what, p.Name)
			}
			parts = append(parts, provider.Part{Type: provider.PartText, Text: fmt.Sprintf("[Earlier attachment %q can't be read by the current model.]", p.Name)})
			changed = true
		}
		if !changed {
			continue
		}
		if out == nil {
			out = append([]provider.Message(nil), messages...)
		}
		out[i] = provider.Message{Role: m.Role, Content: m.Content, Parts: parts}
	}
	if out == nil {
		return messages, nil
	}
	return out, nil
}

func modelAccepts(model router.Model, modality string) bool {
	for _, m := range model.InputModalities {
		if m == modality {
			return true
		}
	}
	return false
}

func modelName(model router.Model) string {
	if model.DisplayName != "" {
		return model.DisplayName
	}
	return model.ID
}

func attachmentIDs(refs []conversation.Attachment) []string {
	if len(refs) == 0 {
		return nil
	}
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = r.ID
	}
	return ids
}

// withAttachmentNotes gives the summarizer a mention of each file, since it
// only ever sees text: without it a message that was just a file would
// fold into the summary as nothing.
func withAttachmentNotes(messages []conversation.Message) []conversation.Message {
	out := make([]conversation.Message, len(messages))
	for i, m := range messages {
		out[i] = m
		if len(m.Attachments) == 0 {
			continue
		}
		names := make([]string, len(m.Attachments))
		for j, a := range m.Attachments {
			names[j] = a.Name
		}
		out[i].Content = strings.TrimSpace(m.Content + "\n[Attached: " + strings.Join(names, ", ") + "]")
	}
	return out
}

// RunCleanup deletes what would otherwise pile up -- once shortly after
// start and then every interval, until ctx is done:
//   - uploads never sent (after attachment.UnclaimedTTL) and files left
//     behind by deleted chats; upload-time pruning only covers the
//     uploading user, so a user who never uploads again would keep their
//     abandoned files forever;
//   - stored web pages older than pagestore.TTL, pages of chats that were
//     never stored or are gone, and the oldest pages beyond
//     PageStoreMaxBytes;
//   - expired browser sessions, and sessions whose API key was deleted.
func (s *Server) RunCleanup(ctx context.Context, interval time.Duration) {
	if s.Attachments == nil && s.Pages == nil && s.Sessions == nil {
		return
	}
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.cleanupOnce(ctx)
		timer.Reset(interval)
	}
}

// PageStoreMaxBytes is the size budget for all stored web pages.
const PageStoreMaxBytes = pagestore.DefaultMaxBytes

func (s *Server) cleanupOnce(ctx context.Context) {
	sweepCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	now := time.Now()
	if s.Attachments != nil {
		deleted, err := s.Attachments.Sweep(sweepCtx, now.Add(-attachment.UnclaimedTTL))
		if err != nil {
			log.Printf("server: attachment sweep: %v", err)
		} else if deleted > 0 {
			log.Printf("server: attachment sweep deleted %d files", deleted)
		}
	}
	if s.Pages != nil {
		deleted, err := s.Pages.Sweep(sweepCtx, now.Add(-pagestore.TTL), now.Add(-pagestore.OrphanGrace), PageStoreMaxBytes)
		if err != nil {
			log.Printf("server: web page sweep: %v", err)
		} else if deleted > 0 {
			log.Printf("server: web page sweep deleted %d pages", deleted)
		}
	}
	if s.Sessions != nil {
		deleted, err := s.Sessions.SweepSessions(sweepCtx, now)
		if err != nil {
			log.Printf("server: session sweep: %v", err)
		} else if deleted > 0 {
			log.Printf("server: session sweep deleted %d sessions", deleted)
		}
	}
}
