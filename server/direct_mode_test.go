package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"neochat/attachment"
	"neochat/auth"
	"neochat/classifier"
	"neochat/conversation"
	"neochat/costlog"
	"neochat/idempotency"
	"neochat/limits"
	"neochat/moderation"
	"neochat/provider"
	"neochat/router"
)

// polzaStandIn is a local Polza-compatible endpoint that records what each
// chat-completions call carried.
type polzaStandIn struct {
	mu       sync.Mutex
	auth     []string
	bodies   []map[string]any
	status   int
	errBody  string
	reply    string
	holdOpen chan struct{} // if set, stream the first word then block until closed or the client leaves
	// chunks are raw SSE data lines sent before the reply (server-tool
	// events, annotations, usage) to imitate a web-search answer.
	chunks []string
	// rejectWeb refuses any request carrying web settings (function tools
	// or the web plugin), like a vendor that doesn't accept them, with
	// rejectStatus (default 400).
	rejectWeb    bool
	rejectStatus int
}

func (p *polzaStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	p.mu.Lock()
	p.auth = append(p.auth, r.Header.Get("Authorization"))
	p.bodies = append(p.bodies, body)
	p.mu.Unlock()
	if p.rejectWeb && (body["tools"] != nil || body["plugins"] != nil) {
		status := p.rejectStatus
		if status == 0 {
			status = http.StatusBadRequest
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"code":"BAD_REQUEST","message":"tools are not supported"}}`))
		return
	}
	if p.status != 0 {
		w.WriteHeader(p.status)
		_, _ = w.Write([]byte(p.errBody))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	for _, chunk := range p.chunks {
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		flusher.Flush()
	}
	words := strings.SplitAfter(p.reply, " ")
	for i, word := range words {
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", word)
		flusher.Flush()
		if i == 0 && p.holdOpen != nil {
			select {
			case <-p.holdOpen:
			case <-r.Context().Done():
				return
			}
		}
	}
	fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
}

func (p *polzaStandIn) lastBody(t *testing.T) map[string]any {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.bodies) == 0 {
		t.Fatal("no request reached the vendor")
	}
	return p.bodies[len(p.bodies)-1]
}

func newDirectServer(t *testing.T, standIn *polzaStandIn) *Server {
	t.Helper()
	vendor := httptest.NewServer(standIn)
	t.Cleanup(vendor.Close)
	client := provider.NewPolzaClient("")
	client.BaseURL = vendor.URL

	catalog := router.Catalog{Models: []router.Model{
		{ID: "gpt-6-luna", DisplayName: "GPT-6 Luna", APIModelID: "openai/gpt-6-luna", Provider: "openai", Modes: []string{"instant"}, Reasoning: "effort", InputModalities: []string{"text", "image", "file"}, ToolCalling: true, CostInputPerMTok: 0.05, CostOutputPerMTok: 0.25, ContextWindow: 1_000_000, MaxOutputTokens: 16000},
		{ID: "deepseek-text", DisplayName: "DeepSeek Text", APIModelID: "deepseek/text", Provider: "deepseek", Modes: []string{"instant"}, InputModalities: []string{"text"}, CostInputPerMTok: 0.1, CostOutputPerMTok: 0.2, ContextWindow: 1_000_000, MaxOutputTokens: 16000},
		{ID: "claude-opus-5.5", APIModelID: "anthropic/claude-opus-5.5", Provider: "anthropic", Modes: []string{"max"}, Reasoning: "adaptive", CostInputPerMTok: 4, CostOutputPerMTok: 20, ContextWindow: 1_000_000, MaxOutputTokens: 16000},
		{ID: "claude-haiku-4-5", APIModelID: "anthropic/claude-haiku-4.5", Provider: "anthropic", Modes: []string{"instant"}, CostInputPerMTok: 0.4, CostOutputPerMTok: 0.4, ContextWindow: 200_000, MaxOutputTokens: 16000},
	}}
	// Classifier/moderator fakes have nothing scripted: any call panics,
	// which is how these tests prove direct mode never reaches them.
	return &Server{
		RouterDisabled: true,
		Router:         router.NewRouter(catalog, testWeights()),
		Classifier:     classifier.New(&provider.FakeClient{}, "classifier", "system"),
		Moderator:      moderation.New(&provider.FakeClient{}, "moderator", "system"),
		ModerationLog:  moderation.NewInMemoryBlockLog(),
		Conversations:  conversation.NewInMemoryStore(),
		Store:          limits.NewInMemorySpendStore(),
		Idempotency:    idempotency.NewInMemoryStore(),
		CostLog:        costlog.NewInMemoryStore(),
		Plans:          map[string]limits.PlanLimits{"pro": {PlanID: "pro", ThinkingMaxCapUSD: 10, InstantExtraCapUSD: 3}},
		SystemPrompts:  map[string]string{"Expert": "expert persona prompt"},
		Generators:     map[string]provider.Client{"openai": client, "anthropic": client, "deepseek": client},
		Auth:           auth.NewInMemoryStore(),
		Attachments:    attachment.NewInMemoryStore(),
	}
}

func collectStream(s *Server, ctx context.Context, req chatRequest) (map[string][]any, error) {
	events := map[string][]any{}
	var mu sync.Mutex
	err := s.handleStream(ctx, req, s.Plans["pro"], func(event string, payload any) {
		mu.Lock()
		events[event] = append(events[event], payload)
		mu.Unlock()
	})
	return events, err
}

func TestDirectMode_RejectsNonManualModes(t *testing.T) {
	standIn := &polzaStandIn{reply: "hi"}
	s := newDirectServer(t, standIn)
	for _, mode := range []string{"auto", "instant", "thinking", "max"} {
		req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hello", RequestedMode: mode, providerKey: "pza_user"}
		_, err := collectStream(s, context.Background(), req)
		if !errors.Is(err, errRouterDisabled) {
			t.Fatalf("mode %s: err = %v, want errRouterDisabled", mode, err)
		}
		if got := clientErrorMessage(err); !strings.HasPrefix(got, "Router disabled") {
			t.Fatalf("mode %s: client message = %q", mode, got)
		}
	}
	if len(standIn.bodies) != 0 {
		t.Fatalf("%d vendor calls for rejected modes, want 0", len(standIn.bodies))
	}
}

func TestDirectMode_RequiresProviderKey(t *testing.T) {
	s := newDirectServer(t, &polzaStandIn{reply: "hi"})
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hello", RequestedMode: "manual", ManualModelID: "gpt-6-luna"}
	_, err := collectStream(s, context.Background(), req)
	if !errors.Is(err, errMissingProviderKey) {
		t.Fatalf("err = %v, want errMissingProviderKey", err)
	}
	if !strings.Contains(clientErrorMessage(err), "Polza AI API key") {
		t.Fatalf("client message = %q", clientErrorMessage(err))
	}
}

func TestDirectMode_SendsKeyEffortPersonaAndInstructions(t *testing.T) {
	standIn := &polzaStandIn{reply: "direct answer"}
	s := newDirectServer(t, standIn)
	req := chatRequest{
		UserID: "u1", PlanID: "pro", Message: "hello", RequestedMode: "manual", ManualModelID: "gpt-6-luna",
		Persona: "Expert", Instructions: "Answer in Russian.", ReasoningEffort: "xhigh", providerKey: "pza_user",
	}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events["done"]) != 1 {
		t.Fatalf("events = %v, want one done", events)
	}
	if done := events["done"][0].(chatResponse); done.ResponseText != "direct answer" || done.SelectedModelID != "gpt-6-luna" {
		t.Fatalf("done = %+v", done)
	}
	if standIn.auth[0] != "Bearer pza_user" {
		t.Fatalf("Authorization = %q, want the user's key", standIn.auth[0])
	}
	body := standIn.lastBody(t)
	if body["model"] != "openai/gpt-6-luna" {
		t.Fatalf("model = %v", body["model"])
	}
	if reasoning, _ := body["reasoning"].(map[string]any); reasoning["effort"] != "xhigh" {
		t.Fatalf("reasoning = %v, want effort xhigh", body["reasoning"])
	}
	messages, _ := body["messages"].([]any)
	var system []string
	for _, raw := range messages {
		m := raw.(map[string]any)
		if m["role"] == "system" {
			system = append(system, m["content"].(string))
		}
	}
	if len(system) != 2 || system[0] != "expert persona prompt" || !strings.Contains(system[1], "Answer in Russian.") {
		t.Fatalf("system messages = %q, want persona then instructions", system)
	}
}

func TestDirectMode_AdaptiveAndUnsupportedReasoning(t *testing.T) {
	standIn := &polzaStandIn{reply: "ok"}
	s := newDirectServer(t, standIn)
	base := chatRequest{UserID: "u1", PlanID: "pro", Message: "hello", RequestedMode: "manual", ReasoningEffort: "xhigh", providerKey: "pza_user"}

	req := base
	req.ManualModelID = "claude-opus-5.5"
	if _, err := collectStream(s, context.Background(), req); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := standIn.lastBody(t)["reasoning"].(map[string]any)
	if reasoning["type"] != "adaptive" || reasoning["effort_level"] != "max" {
		t.Fatalf("adaptive reasoning = %v, want type=adaptive effort_level=max", reasoning)
	}

	req = base
	req.ManualModelID = "claude-haiku-4-5"
	if _, err := collectStream(s, context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, ok := standIn.lastBody(t)["reasoning"]; ok {
		t.Fatal("a model without reasoning control must not get a reasoning block")
	}
}

func TestDirectMode_RejectsBadEffortAndHugeInstructions(t *testing.T) {
	s := newDirectServer(t, &polzaStandIn{reply: "ok"})
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hello", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", ReasoningEffort: "ultra"}
	if _, err := collectStream(s, context.Background(), req); !errors.Is(err, errInvalidRequest) {
		t.Fatalf("bad effort: err = %v", err)
	}
	req.ReasoningEffort = ""
	req.Instructions = strings.Repeat("x", maxInstructionsRunes+1)
	if _, err := collectStream(s, context.Background(), req); !errors.Is(err, errInvalidRequest) {
		t.Fatalf("huge instructions: err = %v", err)
	}
}

func TestDirectMode_VendorRefusalIsShownToUser(t *testing.T) {
	standIn := &polzaStandIn{status: http.StatusUnauthorized, errBody: `{"error":{"code":"UNAUTHORIZED","message":"bad key"}}`}
	s := newDirectServer(t, standIn)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hello", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_bad"}
	_, err := collectStream(s, context.Background(), req)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := clientErrorMessage(err); !strings.Contains(got, "rejected your API key") {
		t.Fatalf("client message = %q", got)
	}
	spent, _ := s.Store.Sum(context.Background(), "u1", limits.PoolInstant, time.Hour)
	if spent != 0 {
		t.Fatalf("a refused call left %v reserved, want 0", spent)
	}
}

func TestDirectMode_StopKeepsPartialReply(t *testing.T) {
	standIn := &polzaStandIn{reply: "partial answer that never finishes", holdOpen: make(chan struct{})}
	defer close(standIn.holdOpen)
	s := newDirectServer(t, standIn)

	ctx, cancel := context.WithCancel(context.Background())
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hello", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user"}
	var conversationID string
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.handleStream(ctx, req, s.Plans["pro"], func(event string, payload any) {
			switch event {
			case "meta":
				conversationID = payload.(map[string]any)["conversation_id"].(string)
			case "delta":
				cancel() // the user pressed Stop after the first chunk
			}
		})
	}()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected the stopped stream to end with an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stopping did not end the stream")
	}

	history, err := s.Conversations.History(context.Background(), "u1", conversationID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Content != "hello" || history[1].Content != "partial " {
		t.Fatalf("history = %+v, want the user message and the partial reply", history)
	}
}

func TestDirectMode_StopSettlesOnEstimateNotUpperBound(t *testing.T) {
	standIn := &polzaStandIn{reply: "partial answer that never finishes", holdOpen: make(chan struct{})}
	defer close(standIn.holdOpen)
	s := newDirectServer(t, standIn)

	ctx, cancel := context.WithCancel(context.Background())
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hello", RequestedMode: "manual", ManualModelID: "claude-opus-5.5", providerKey: "pza_user"}
	_ = s.handleStream(ctx, req, s.Plans["pro"], func(event string, _ any) {
		if event == "delta" {
			cancel()
		}
	})
	spent, err := s.Store.Sum(context.Background(), "u1", limits.PoolThinkingMax, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	upperBound := router.ComputeCostUSDRates(4, 20, 0, 16000)
	if spent <= 0 || spent >= upperBound/10 {
		t.Fatalf("stopped generation charged %v, want a small positive estimate (upper bound %v)", spent, upperBound)
	}
}
