package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStream_ReasoningIsStreamedAndKept(t *testing.T) {
	standIn := &polzaStandIn{reply: "the answer", thinking: []string{"Let me ", "think."}}
	s, url, token, _ := streamServer(t, standIn)
	stream := openStream(t, "POST", url+"/chat/stream", token, chatBody("r1"), nil)
	var thought strings.Builder
	var done chatResponse
	var conversationID string
	for {
		event := stream.next()
		switch event.name {
		case "meta":
			conversationID = conversationOf(t, event)
		case "reasoning":
			var piece struct{ Text string }
			_ = json.Unmarshal([]byte(event.data), &piece)
			thought.WriteString(piece.Text)
		case "done":
			_ = json.Unmarshal([]byte(event.data), &done)
		}
		if event.name == "done" || event.name == "error" {
			break
		}
	}
	if thought.String() != "Let me think." || done.Reasoning != "Let me think." || done.ResponseText != "the answer" {
		t.Fatalf("reasoning events %q, done %+v", thought.String(), done)
	}
	history, _ := s.Conversations.History(context.Background(), "u1", conversationID, 0)
	if len(history) != 2 || history[1].Versions[0].Reasoning != "Let me think." || history[1].Content != "the answer" {
		t.Fatalf("stored = %+v", history)
	}
}

func TestLive_OtherDevicesHearAboutReplies(t *testing.T) {
	standIn := &polzaStandIn{reply: "shared answer here", holdOpen: make(chan struct{})}
	s, url, token, other := streamServer(t, standIn)

	phone := openEvents(t, url, token)
	strangers := openEvents(t, url, other)
	stream := openStream(t, "POST", url+"/chat/stream", token, chatBody("l1"), nil)
	meta, _ := stream.until("meta")
	started := phone.until("started")
	var notice liveNotice
	_ = json.Unmarshal([]byte(started.data), &notice)
	if notice.StreamID != stream.id || notice.ConversationID != conversationOf(t, meta) || notice.Message != "hello" || notice.ModelID != "gpt-6-luna" {
		t.Fatalf("started = %+v", notice)
	}

	// A device opening the app now hears about the running reply first,
	// and can read the chat while it streams.
	laptop := openEvents(t, url, token)
	var running liveNotice
	_ = json.Unmarshal([]byte(laptop.until("started").data), &running)
	if running.StreamID != stream.id {
		t.Fatalf("running = %+v", running)
	}
	if code := getJSON(t, s, "GET", "/conversations/"+notice.ConversationID, token, nil, nil); code != http.StatusOK {
		t.Fatalf("reading the chat during a reply: %d", code)
	}
	if code := getJSON(t, s, "GET", "/conversations", token, nil, nil); code != http.StatusOK {
		t.Fatalf("listing chats during a reply: %d", code)
	}

	close(standIn.holdOpen)
	stream.until("done")
	finished := phone.until("finished")
	if !strings.Contains(finished.data, stream.id) {
		t.Fatalf("finished = %+v", finished)
	}
	laptop.until("finished")

	// Nothing reached the other user.
	strangers.drop()
	if strangers.sawAny("started", "finished") {
		t.Fatal("another user heard about the reply")
	}
}

func TestLive_IncognitoStaysOnItsDevice(t *testing.T) {
	standIn := &polzaStandIn{reply: "private answer"}
	_, url, token, _ := streamServer(t, standIn)
	events := openEvents(t, url, token)
	body := `{"message":"secret","requested_mode":"manual","manual_model_id":"gpt-6-luna","incognito":true,"idempotency_key":"i1"}`
	stream := openStream(t, "POST", url+"/chat/stream", token, body, nil)
	stream.until("done")
	time.Sleep(100 * time.Millisecond)
	events.drop()
	if events.sawAny("started", "finished") {
		t.Fatal("an incognito reply was announced")
	}
}

// liveEvents reads GET /events.
type liveEvents struct {
	*liveStream
	seen []sseEvent
}

func openEvents(t *testing.T, url, token string) *liveEvents {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, "GET", url+"/events", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("GET /events: %v %v", err, response)
	}
	stream := &liveStream{t: t, body: response.Body, cancel: cancel}
	stream.reader = newLineReader(response.Body)
	t.Cleanup(stream.drop)
	return &liveEvents{liveStream: stream}
}

func (e *liveEvents) until(name string) sseEvent {
	e.t.Helper()
	for {
		event := e.next()
		e.seen = append(e.seen, event)
		if event.name == name {
			return event
		}
	}
}

// sawAny reads what's left (after drop, until EOF) and reports whether any
// of names came.
func (e *liveEvents) sawAny(names ...string) bool {
	for {
		line, err := e.reader.ReadString('\n')
		for _, name := range names {
			if strings.TrimSpace(line) == "event: "+name {
				return true
			}
		}
		if err != nil {
			break
		}
	}
	for _, event := range e.seen {
		for _, name := range names {
			if event.name == name {
				return true
			}
		}
	}
	return false
}
