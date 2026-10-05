package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"neochat/auth"
)

// Live replies on every device. GET /events is one long-lived stream per
// open app. It tells each of a user's devices when one of their replies
// starts streaming ("started", with what was asked), when it ends
// ("finished"), and when a chat was opened somewhere ("read"). A device showing that chat follows the reply through
// GET /chat/stream/{stream_id} (streamjob.go); the others mark the chat as
// live and refresh the list once it's done. A device that connects while
// replies are running hears about them first. Incognito replies stay on
// the device that asked.

const (
	liveHeartbeat = 25 * time.Second
	// maxLiveWatchers bounds the open /events streams per user: tabs and
	// devices.
	maxLiveWatchers = 8
)

// liveNotice is what other devices learn about a running reply.
type liveNotice struct {
	StreamID       string `json:"stream_id"`
	ConversationID string `json:"conversation_id"`
	// Message and Quote are the user's new message; a regeneration
	// carries RegenerateMessageID instead (the answer being replaced).
	Message             string `json:"message,omitempty"`
	Quote               string `json:"quote,omitempty"`
	RegenerateMessageID int64  `json:"regenerate_message_id,omitempty"`
	ModelID             string `json:"model_id,omitempty"`
}

type liveEvent struct {
	name   string
	notice liveNotice
}

// setConversation records the chat the job answers in; true the first
// time.
func (j *streamJob) setConversation(id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.conversationID != "" {
		return false
	}
	j.conversationID = id
	return true
}

func (j *streamJob) notice() liveNotice {
	j.mu.Lock()
	defer j.mu.Unlock()
	notice := j.asked
	notice.StreamID, notice.ConversationID = j.id, j.conversationID
	return notice
}

// running reports the job's notice while it runs in a known, announced
// chat.
func (j *streamJob) running() (liveNotice, bool) {
	j.mu.Lock()
	known := !j.private && !j.done && j.conversationID != ""
	j.mu.Unlock()
	if !known {
		return liveNotice{}, false
	}
	return j.notice(), true
}

// watch subscribes to a user's live events; it also returns the replies
// already running and the channel closed on shutdown. false: too many
// open already.
func (r *streamRegistry) watch(userID string) (chan liveEvent, []liveNotice, <-chan struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing == nil {
		r.closing = make(chan struct{})
	}
	if len(r.watchers[userID]) >= maxLiveWatchers {
		return nil, nil, nil, false
	}
	if r.watchers == nil {
		r.watchers = map[string]map[chan liveEvent]struct{}{}
	}
	if r.watchers[userID] == nil {
		r.watchers[userID] = map[chan liveEvent]struct{}{}
	}
	ch := make(chan liveEvent, 32)
	r.watchers[userID][ch] = struct{}{}
	var running []liveNotice
	for _, job := range r.byID {
		if job.userID != userID {
			continue
		}
		if notice, ok := job.running(); ok {
			running = append(running, notice)
		}
	}
	return ch, running, r.closing, true
}

func (r *streamRegistry) unwatch(userID string, ch chan liveEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.watchers[userID], ch)
	if len(r.watchers[userID]) == 0 {
		delete(r.watchers, userID)
	}
}

// announce tells the user's watchers; one too far behind misses it (it
// catches up on its next connect).
func (r *streamRegistry) announce(userID, name string, notice liveNotice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.watchers[userID] {
		select {
		case ch <- liveEvent{name: name, notice: notice}:
		default:
		}
	}
}

// announceEnd says "finished" for a job that was announced.
func (r *streamRegistry) announceEnd(job *streamJob) {
	if job.private {
		return
	}
	if notice := job.notice(); notice.ConversationID != "" {
		r.announce(job.userID, "finished", notice)
	}
}

// closeWatchers ends every /events stream (shutdown: they never end on
// their own, and the client reconnects).
func (r *streamRegistry) closeWatchers() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing == nil {
		r.closing = make(chan struct{})
	}
	if !r.closed {
		r.closed = true
		close(r.closing)
	}
}

// CloseLive ends the open GET /events streams; register it with
// http.Server.RegisterOnShutdown.
func (s *Server) CloseLive() { s.streams.closeWatchers() }

// handleLiveEvents is GET /events: the user's live events as Server-Sent
// Events, the replies already running first, a comment every
// liveHeartbeat.
func (s *Server) handleLiveEvents(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, running, closing, ok := s.streams.watch(identity.UserID)
	if !ok {
		http.Error(w, "too many open NeoChat windows", http.StatusTooManyRequests)
		return
	}
	defer s.streams.unwatch(identity.UserID, ch)
	controller := http.NewResponseController(w)
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(name string, notice liveNotice) bool {
		data, _ := json.Marshal(notice)
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, notice := range running {
		if !write("started", notice) {
			return
		}
	}
	_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprint(w, ": ready\n\n"); err != nil {
		return
	}
	flusher.Flush()
	heartbeat := time.NewTicker(liveHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case event := <-ch:
			if !write(event.name, event.notice) {
				return
			}
		case <-heartbeat.C:
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-closing:
			return
		case <-r.Context().Done():
			return
		}
	}
}
