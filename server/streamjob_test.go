package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"neochat/auth"
)

type sseEvent struct {
	id   int
	name string
	data string
}

// parseSSE splits a Server-Sent Events transcript into its events
// (comments dropped).
func parseSSE(transcript string) []sseEvent {
	var events []sseEvent
	for _, block := range strings.Split(transcript, "\n\n") {
		var event sseEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "id: "):
				event.id, _ = strconv.Atoi(strings.TrimPrefix(line, "id: "))
			case strings.HasPrefix(line, "event: "):
				event.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				event.data = strings.TrimPrefix(line, "data: ")
			}
		}
		if event.name != "" {
			events = append(events, event)
		}
	}
	return events
}

// liveStream reads a real SSE response event by event.
type liveStream struct {
	t      *testing.T
	body   io.ReadCloser
	reader *bufio.Reader
	cancel context.CancelFunc
	id     string // stream_id
	last   int    // last numbered event read
}

func openStream(t *testing.T, method, url, token, body string, headers map[string]string) *liveStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(providerKeyHeader, "pza_user")
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(response.Body)
		cancel()
		t.Fatalf("%s %s: %d %s", method, url, response.StatusCode, raw)
	}
	stream := &liveStream{t: t, body: response.Body, reader: newLineReader(response.Body), cancel: cancel}
	if first := stream.next(); first.name != "stream" {
		t.Fatalf("first event = %+v, want stream", first)
	} else {
		var named struct {
			StreamID string `json:"stream_id"`
		}
		_ = json.Unmarshal([]byte(first.data), &named)
		stream.id = named.StreamID
	}
	return stream
}

// next is the next event; io.EOF ends the test.
func (s *liveStream) next() sseEvent {
	s.t.Helper()
	var event sseEvent
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			s.t.Fatalf("stream ended: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		if line == "" {
			if event.name != "" {
				if event.id > 0 {
					s.last = event.id
				}
				return event
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			event.id, _ = strconv.Atoi(strings.TrimPrefix(line, "id: "))
		case strings.HasPrefix(line, "event: "):
			event.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			event.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// until reads up to the first event named one of names.
func (s *liveStream) until(names ...string) (sseEvent, string) {
	s.t.Helper()
	var text strings.Builder
	for {
		event := s.next()
		if event.name == "delta" {
			var delta struct{ Text string }
			_ = json.Unmarshal([]byte(event.data), &delta)
			text.WriteString(delta.Text)
		}
		for _, name := range names {
			if event.name == name {
				return event, text.String()
			}
		}
	}
}

func newLineReader(body io.Reader) *bufio.Reader { return bufio.NewReader(body) }

func (s *liveStream) drop() {
	s.cancel()
	s.body.Close()
}

func streamServer(t *testing.T, standIn *polzaStandIn) (*Server, string, string, string) {
	t.Helper()
	s := newDirectServer(t, standIn)
	store := s.Auth.(*auth.InMemoryStore)
	token, _ := store.IssueKey(context.Background(), "u1", "pro")
	other, _ := store.IssueKey(context.Background(), "u2", "pro")
	web := httptest.NewServer(s.Mux())
	t.Cleanup(web.Close)
	return s, web.URL, token, other
}

func chatBody(key string) string {
	body := map[string]any{"message": "hello", "requested_mode": "manual", "manual_model_id": "gpt-6-luna"}
	if key != "" {
		body["idempotency_key"] = key
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

func storedReply(t *testing.T, s *Server, conversationID string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		history, err := s.Conversations.History(context.Background(), "u1", conversationID, 0)
		if err == nil && len(history) == 2 {
			return history[1].Content
		}
		if time.Now().After(deadline) {
			t.Fatalf("no stored reply: %v %+v", err, history)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func conversationOf(t *testing.T, meta sseEvent) string {
	t.Helper()
	var m struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal([]byte(meta.data), &m); err != nil || m.ConversationID == "" {
		t.Fatalf("meta = %+v", meta)
	}
	return m.ConversationID
}

func TestStream_SurvivesADroppedConnectionAndResumes(t *testing.T) {
	standIn := &polzaStandIn{reply: "the whole answer arrives", holdOpen: make(chan struct{})}
	s, url, token, other := streamServer(t, standIn)

	stream := openStream(t, "POST", url+"/chat/stream", token, chatBody("k1"), nil)
	meta, _ := stream.until("meta")
	_, first := stream.until("delta")
	if first != "the " {
		t.Fatalf("first delta = %q", first)
	}
	stream.drop() // the phone went to the background
	time.Sleep(100 * time.Millisecond)
	close(standIn.holdOpen) // the model goes on

	if code := getJSON(t, s, "GET", "/chat/stream/"+stream.id, other, nil, nil); code != http.StatusNotFound {
		t.Fatalf("another user resumed the stream: %d", code)
	}
	resumed := openStream(t, "GET", fmt.Sprintf("%s/chat/stream/%s?after=%d", url, stream.id, stream.last), token, "", nil)
	done, rest := resumed.until("done")
	if rest != "whole answer arrives" {
		t.Fatalf("resumed deltas = %q, want what the dropped connection missed", rest)
	}
	var response chatResponse
	_ = json.Unmarshal([]byte(done.data), &response)
	if response.ResponseText != "the whole answer arrives" {
		t.Fatalf("done = %+v", response)
	}
	if got := storedReply(t, s, conversationOf(t, meta)); got != "the whole answer arrives" {
		t.Fatalf("stored reply = %q, want the full answer, not a stopped one", got)
	}

	// Long after, from the start: everything again.
	again := openStream(t, "GET", url+"/chat/stream/"+stream.id, token, "", nil)
	if _, all := again.until("done"); all != "the whole answer arrives" {
		t.Fatalf("full replay = %q", all)
	}
}

func TestStream_RetriedPostAttachesToTheRunningAnswer(t *testing.T) {
	standIn := &polzaStandIn{reply: "one answer only", holdOpen: make(chan struct{})}
	_, url, token, _ := streamServer(t, standIn)

	first := openStream(t, "POST", url+"/chat/stream", token, chatBody("k2"), nil)
	first.until("delta")
	first.drop()
	retry := openStream(t, "POST", url+"/chat/stream", token, chatBody("k2"), map[string]string{"Last-Event-ID": strconv.Itoa(first.last)})
	if retry.id != first.id {
		t.Fatalf("retry got stream %s, want the running %s", retry.id, first.id)
	}
	close(standIn.holdOpen)
	if _, rest := retry.until("done", "error"); rest != "answer only" {
		t.Fatalf("retry deltas = %q", rest)
	}
	standIn.mu.Lock()
	calls := len(standIn.bodies)
	standIn.mu.Unlock()
	if calls != 1 {
		t.Fatalf("%d vendor calls, want the one answer", calls)
	}
}

func TestStream_StopEndsItAndKeepsThePartialReply(t *testing.T) {
	standIn := &polzaStandIn{reply: "partial answer that never finishes", holdOpen: make(chan struct{})}
	defer close(standIn.holdOpen)
	s, url, token, _ := streamServer(t, standIn)

	stream := openStream(t, "POST", url+"/chat/stream", token, chatBody("k3"), nil)
	meta, _ := stream.until("meta")
	stream.until("delta")
	request := httptest.NewRequest("POST", "/chat/stop", strings.NewReader(`{"idempotency_key":"k3"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	s.Mux().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("stop: %d %s", response.Code, response.Body)
	}
	// Stop answers once the partial reply is stored.
	history, _ := s.Conversations.History(context.Background(), "u1", conversationOf(t, meta), 0)
	if len(history) != 2 || history[1].Content != "partial " {
		t.Fatalf("history right after stop = %+v", history)
	}
	if last, _ := stream.until("stopped", "done", "error"); last.name != "stopped" {
		t.Fatalf("stream ended with %+v, want stopped", last)
	}
}

func TestStream_StopThatOvertakesItsRequest(t *testing.T) {
	standIn := &polzaStandIn{reply: "never wanted"}
	s, url, token, _ := streamServer(t, standIn)
	request := httptest.NewRequest("POST", "/chat/stop", strings.NewReader(`{"idempotency_key":"k4"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	s.Mux().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("stop: %d", response.Code)
	}
	stream := openStream(t, "POST", url+"/chat/stream", token, chatBody("k4"), nil)
	if last, _ := stream.until("stopped", "done", "error"); last.name != "stopped" {
		t.Fatalf("stream ended with %+v, want stopped", last)
	}
}

func TestStream_WithoutAKeyLeavingStillStops(t *testing.T) {
	standIn := &polzaStandIn{reply: "partial answer that never finishes", holdOpen: make(chan struct{})}
	defer close(standIn.holdOpen)
	s, url, token, _ := streamServer(t, standIn)
	stream := openStream(t, "POST", url+"/chat/stream", token, chatBody(""), nil)
	meta, _ := stream.until("meta")
	stream.until("delta")
	stream.drop()
	if got := storedReply(t, s, conversationOf(t, meta)); got != "partial " {
		t.Fatalf("stored reply = %q, want the partial one", got)
	}
}

func TestStreamRegistry_ForgetsOldStreams(t *testing.T) {
	var r streamRegistry
	now := time.Now()
	var jobs []*streamJob
	for i := 0; i < streamsPerUser+2; i++ {
		job, _, _ := r.open("u1", fmt.Sprintf("k%d", i), func() {}, liveNotice{}, false, now)
		job.finish()
		job.mu.Lock()
		job.finished = now.Add(time.Duration(i) * time.Second)
		job.mu.Unlock()
		jobs = append(jobs, job)
	}
	if r.find("u1", jobs[0].id, "") != nil || r.find("u1", jobs[1].id, "") != nil || r.find("u1", jobs[2].id, "") == nil {
		t.Fatal("a user keeps only their newest finished streams")
	}
	r.mu.Lock()
	r.sweepLocked(now.Add(streamRetention + time.Hour))
	left := len(r.byID) + len(r.byKey)
	r.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d entries left after the retention", left)
	}
}
