package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"neochat/auth"
	"neochat/conversation"
	"neochat/limits"
)

// Streams outlive their connection. A chat request that carries an
// idempotency key (the web UI's always do) runs as a job detached from the
// HTTP request that started it: a phone that puts the app in the
// background, or a network that drops, cuts the connection but not the
// answer. The job keeps every event it sent, numbered (the SSE id), so the
// client comes back with GET /chat/stream/{stream_id}?after=N and gets what
// it missed, then the rest live. A POST retried with the same idempotency
// key attaches to the job instead of starting over. Since leaving no longer
// stops such a stream, Stop is explicit: POST /chat/stop.
//
// A request without a key keeps the old contract: the stream is tied to
// its connection, and leaving stops it.
//
// Jobs live in this process's memory and stay replayable for
// streamRetention after they finish -- long enough to come back to a
// switched-away app. After that the stored chat has the answer.

const (
	streamRetention = 15 * time.Minute
	// streamsPerUser bounds the finished streams kept for one user.
	streamsPerUser = 4
	// streamHeartbeat keeps a quiet stream (a long think) visibly alive,
	// for proxies that drop idle connections and for the client's
	// watchdog.
	streamHeartbeat = 15 * time.Second
	// stopTombstone is how long a Stop for a stream that hasn't started
	// yet is remembered: one that overtakes its own request still stops it.
	stopTombstone = time.Minute
	// stopWait is how long POST /chat/stop waits for the stopped answer to
	// be stored.
	stopWait = 10 * time.Second
)

type jobEvent struct {
	name string
	data []byte
}

type streamJob struct {
	id     string
	userID string
	key    string
	cancel context.CancelFunc
	ended  chan struct{} // closed once the job finishes

	mu       sync.Mutex
	events   []jobEvent
	done     bool
	finished time.Time
	wake     chan struct{} // closed and replaced on every change
}

// send is the job's event sink (handleStream's send): it numbers and keeps
// the event and wakes everyone following the job.
func (j *streamJob) send(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("server: stream %s: marshal %s event: %v", j.id, event, err)
		return
	}
	j.mu.Lock()
	j.events = append(j.events, jobEvent{name: event, data: data})
	j.wakeLocked()
	j.mu.Unlock()
}

func (j *streamJob) finish() {
	j.mu.Lock()
	j.done, j.finished = true, time.Now()
	j.wakeLocked()
	j.mu.Unlock()
	close(j.ended)
}

func (j *streamJob) wakeLocked() {
	close(j.wake)
	j.wake = make(chan struct{})
}

// since returns the events after the first `after`, whether the job has
// finished (then the events run to its end), and a channel closed on the
// next change.
func (j *streamJob) since(after int) ([]jobEvent, bool, <-chan struct{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var events []jobEvent
	if after < len(j.events) {
		// Capped, so a later append never writes into what the caller reads.
		events = j.events[after:len(j.events):len(j.events)]
	}
	return events, j.done, j.wake
}

func (j *streamJob) expired(now time.Time) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done && now.Sub(j.finished) > streamRetention
}

// streamRegistry holds this process's stream jobs. The zero value is ready.
type streamRegistry struct {
	mu      sync.Mutex
	byID    map[string]*streamJob
	byKey   map[string]*streamJob
	stopped map[string]time.Time // Stops that came before their stream
	running sync.WaitGroup
}

func streamKey(userID, key string) string { return userID + "\x00" + key }

// open returns the user's job for an idempotency key, running or still
// kept (attached), or else registers a new one that cancel stops. stopped
// reports a Stop that arrived before the request did.
func (r *streamRegistry) open(userID, key string, cancel context.CancelFunc, now time.Time) (job *streamJob, attached, stopped bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(now)
	if key != "" {
		if job := r.byKey[streamKey(userID, key)]; job != nil {
			return job, true, false
		}
	}
	if r.byID == nil {
		r.byID, r.byKey, r.stopped = map[string]*streamJob{}, map[string]*streamJob{}, map[string]time.Time{}
	}
	job = &streamJob{id: conversation.NewID(), userID: userID, key: key, cancel: cancel, ended: make(chan struct{}), wake: make(chan struct{})}
	r.byID[job.id] = job
	if key != "" {
		k := streamKey(userID, key)
		r.byKey[k] = job
		if _, ok := r.stopped[k]; ok {
			delete(r.stopped, k)
			stopped = true
		}
	}
	return job, false, stopped
}

// find is one of the user's jobs, by stream ID or else idempotency key.
func (r *streamRegistry) find(userID, id, key string) *streamJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(time.Now())
	return r.findLocked(userID, id, key)
}

func (r *streamRegistry) findLocked(userID, id, key string) *streamJob {
	if job := r.byID[id]; job != nil && id != "" && job.userID == userID {
		return job
	}
	if key != "" {
		return r.byKey[streamKey(userID, key)]
	}
	return nil
}

// stop cancels one of the user's streams and returns it. A key with no
// stream yet is remembered for stopTombstone.
func (r *streamRegistry) stop(userID, id, key string, now time.Time) *streamJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(now)
	job := r.findLocked(userID, id, key)
	if job == nil {
		if key != "" {
			if r.stopped == nil {
				r.byID, r.byKey, r.stopped = map[string]*streamJob{}, map[string]*streamJob{}, map[string]time.Time{}
			}
			r.stopped[streamKey(userID, key)] = now
		}
		return nil
	}
	job.cancel()
	return job
}

// cancelAll stops every job (shutdown).
func (r *streamRegistry) cancelAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, job := range r.byID {
		job.cancel()
	}
}

// sweepLocked forgets finished jobs past their retention, and all but a
// user's newest streamsPerUser finished ones, and stale early Stops.
func (r *streamRegistry) sweepLocked(now time.Time) {
	finished := map[string][]*streamJob{}
	for id, job := range r.byID {
		if job.expired(now) {
			r.forgetLocked(id, job)
			continue
		}
		job.mu.Lock()
		if job.done {
			finished[job.userID] = append(finished[job.userID], job)
		}
		job.mu.Unlock()
	}
	for _, jobs := range finished {
		for len(jobs) > streamsPerUser {
			oldest := 0
			for i, job := range jobs {
				if job.finished.Before(jobs[oldest].finished) {
					oldest = i
				}
			}
			r.forgetLocked(jobs[oldest].id, jobs[oldest])
			jobs = append(jobs[:oldest], jobs[oldest+1:]...)
		}
	}
	for k, at := range r.stopped {
		if now.Sub(at) > stopTombstone {
			delete(r.stopped, k)
		}
	}
}

func (r *streamRegistry) forgetLocked(id string, job *streamJob) {
	delete(r.byID, id)
	if job.key != "" && r.byKey[streamKey(job.userID, job.key)] == job {
		delete(r.byKey, streamKey(job.userID, job.key))
	}
}

// writeChatStream serves POST /chat/stream and /chat/regenerate/stream:
// it starts the request's job (or attaches to the one already running
// under its idempotency key) and follows it.
func (s *Server) writeChatStream(w http.ResponseWriter, r *http.Request, req chatRequest, plan limits.PlanLimits) {
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if len(req.IdempotencyKey) > 128 {
		http.Error(w, "idempotency_key is too long", http.StatusBadRequest)
		return
	}
	// With a key the client can come back for the stream, so a dropped
	// connection mustn't stop the answer; without one, leaving is how a
	// client stops it, as it always was.
	base := r.Context()
	if req.IdempotencyKey != "" {
		base = context.WithoutCancel(base)
	}
	ctx, cancel := context.WithCancel(base)
	job, attached, stopped := s.streams.open(req.UserID, req.IdempotencyKey, cancel, time.Now())
	if attached {
		cancel()
	} else {
		if stopped {
			cancel()
		}
		s.streams.running.Add(1)
		go s.runStream(ctx, job, req, plan)
	}
	s.followStream(w, r, job, lastEventID(r))
}

// runStream generates one job's answer; its last event says how it ended:
// "done"/"blocked" (from handleStream), "stopped", or "error".
func (s *Server) runStream(ctx context.Context, job *streamJob, req chatRequest, plan limits.PlanLimits) {
	defer s.streams.running.Done()
	defer job.finish()
	defer job.cancel()
	var err error
	func() {
		defer recoverGoroutine("chat stream", &err)
		err = s.handleStream(ctx, req, plan, job.send)
	}()
	switch {
	case err == nil:
	case errors.Is(ctx.Err(), context.Canceled):
		job.send("stopped", map[string]string{})
	default:
		log.Printf("server: /chat/stream error for user_id=%s: %v", req.UserID, err)
		job.send("error", map[string]string{"message": clientErrorMessage(err)})
	}
}

// followStream writes a job's events after the first `after` as
// Server-Sent Events, numbered with id:, then follows it live until it
// finishes or the client leaves. The first event, "stream", names the
// stream for a later GET /chat/stream/{stream_id}; a comment every
// streamHeartbeat keeps a quiet stream alive.
func (s *Server) followStream(w http.ResponseWriter, r *http.Request, job *streamJob, after int) {
	flusher := w.(http.Flusher)
	controller := http.NewResponseController(w)
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(format string, args ...any) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !write("event: stream\ndata: {\"stream_id\":%q}\n\n", job.id) {
		return
	}
	heartbeat := time.NewTicker(streamHeartbeat)
	defer heartbeat.Stop()
	next := max(after, 0)
	for {
		events, done, wake := job.since(next)
		for _, event := range events {
			next++
			if !write("id: %d\nevent: %s\ndata: %s\n\n", next, event.name, event.data) {
				return
			}
		}
		if done {
			return
		}
		select {
		case <-wake:
		case <-heartbeat.C:
			if !write(": ping\n\n") {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// lastEventID is where a client resumes: ?after=N, or the Last-Event-ID
// header a retried POST carries; 0 replays everything.
func lastEventID(r *http.Request) int {
	value := r.URL.Query().Get("after")
	if value == "" {
		value = r.Header.Get("Last-Event-ID")
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// handleStreamResume is GET /chat/stream/{stream_id}?after=N: one of the
// user's streams from after its Nth event, then live. 404 once the stream
// is forgotten (see streamRetention): the stored chat has the answer.
func (s *Server) handleStreamResume(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	job := s.streams.find(identity.UserID, r.PathValue("stream_id"), "")
	if job == nil {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}
	s.followStream(w, r, job, lastEventID(r))
}

type stopStreamRequest struct {
	StreamID       string `json:"stream_id,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// handleStreamStop is POST /chat/stop, the Stop button: it ends one of the
// user's streams, named by stream_id or idempotency_key, and answers once
// the partial reply is stored, so the client can reload the chat right
// after.
func (s *Server) handleStreamStop(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	var req stopStreamRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || (req.StreamID == "" && req.IdempotencyKey == "") || len(req.IdempotencyKey) > 128 {
		http.Error(w, "invalid request body, want {\"stream_id\": …} or {\"idempotency_key\": …}", http.StatusBadRequest)
		return
	}
	if job := s.streams.stop(identity.UserID, req.StreamID, req.IdempotencyKey, time.Now()); job != nil {
		select {
		case <-job.ended:
		case <-time.After(stopWait):
		case <-r.Context().Done():
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// DrainStreams lets running streams finish, up to ctx's deadline, then
// stops the rest so their partial replies are stored. For graceful
// shutdown, after the HTTP server has stopped taking requests.
func (s *Server) DrainStreams(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		s.streams.running.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	s.streams.cancelAll()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}
