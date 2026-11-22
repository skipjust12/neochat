package modelcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"neochat/router"
)

func TestPoeSnapshotKeepsUnlistedBotsSelectableAndRealIDs(t *testing.T) {
	base, err := router.LoadCatalog("../configs/models.json")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadPoe(base, "../configs/poe-models.json")
	if err != nil {
		t.Fatal(err)
	}
	unavailable, ok := catalog.FindModel("gpt-6-luna")
	if !ok || unavailable.Unavailable || !unavailable.Unstable || !unavailable.ManualOnly || unavailable.ResolveAPIModelID() != "GPT-6-Luna" {
		t.Fatalf("unlisted bot lost or blocked: %+v", unavailable)
	}
	active := 0
	for _, m := range catalog.Models {
		if !m.Poe {
			t.Errorf("not Poe: %s", m.ID)
		}
		if m.Unavailable {
			continue
		}
		active++
		if strings.Contains(m.ResolveAPIModelID(), "/") {
			t.Errorf("vendor-prefixed Poe ID: %s", m.ResolveAPIModelID())
		}
		if err := validatePoeModel(m); err != nil {
			t.Error(err)
		}
	}
	if active < 20 {
		t.Fatalf("only %d available models", active)
	}
	opus, ok := catalog.FindModel("claude-opus-4.8")
	if !ok || opus.PoeReasoningParameter != "output_effort" || opus.Reasoning != "adaptive" || opus.CostCacheReadPerMTok == nil {
		t.Fatalf("Claude metadata: %+v", opus)
	}
	nano, ok := catalog.FindModel("nano-banana-2-lite")
	if !ok || nano.ResolveAPIModelID() != "nano-banana-2-lite" || nano.CostPerImageUSD != 0.5 {
		t.Fatalf("media reserve is not an explicit estimate: %+v", nano)
	}
}
func validatePoeModel(m router.Model) error {
	if m.Kind == "" && (m.CostInputPerMTok < 0 || m.CostOutputPerMTok < 0) {
		return fmt.Errorf("bad token rate: %s", m.ID)
	}
	if m.Kind == router.KindVideo && (m.Video == nil || len(m.Video.Durations) == 0 || m.CostPerVideoUSD <= 0 || len(m.Video.Prices) != 0) {
		return fmt.Errorf("bad Poe video metadata: %s", m.ID)
	}
	return nil
}
func TestPoeCatalogTTLAndConcurrentRefresh(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("public catalog received a user key")
		}
		fmt.Fprint(w, `{"data":[{"id":"claude-opus-4.8","owned_by":"Anthropic","created":1790000000000,"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"pricing":{"prompt":"0.000005","completion":"0.000025"},"metadata":{"display_name":"Claude Opus 4.8"},"supported_endpoints":["/v1/messages"]}]}`)
	}))
	defer server.Close()
	now := time.Now()
	base := router.Catalog{Models: []router.Model{{ID: "claude-opus-4.8", Provider: "anthropic", Modes: []string{"max"}, ContextWindow: 200000}}}
	live := NewPoe(base, WithURL(server.URL))
	live.now = func() time.Time { return now }
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := live.Refresh(context.Background(), false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent fetches=%d", calls.Load())
	}
	now = now.Add(59 * time.Minute)
	if _, err := live.Refresh(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Error("catalog refreshed before recommended hour")
	}
	now = now.Add(2 * time.Minute)
	state, err := live.Refresh(context.Background(), false)
	if err != nil || calls.Load() != 2 {
		t.Fatalf("refresh: %d %v", calls.Load(), err)
	}
	m, _ := state.Catalog.FindModel("claude-opus-4.8")
	if m.CostInputPerMTok != 5 || m.CostOutputPerMTok != 25 {
		t.Errorf("USD per token conversion: %+v", m)
	}
}
func TestPoeCatalogRejectsEmptyAndNonfinitePrices(t *testing.T) {
	for _, s := range []string{"NaN", "+Inf", "-1", "", "bad"} {
		if _, ok := poeRate(s); ok {
			t.Errorf("accepted invalid rate %q", s)
		}
	}
	_, err := MergePoe(router.Catalog{}, nil, time.Now())
	if err == nil {
		t.Fatal("empty catalog accepted")
	}
	var m PoeModel
	if err := json.Unmarshal([]byte(`{"id":"bot","pricing":{"prompt":null,"completion":null},"context_window":null}`), &m); err != nil {
		t.Fatal(err)
	}
}

func TestPoeCatalogNewVersionAndUnstableRecovery(t *testing.T) {
	base := router.Catalog{Models: []router.Model{{ID: "claude-sonnet-4.6", DisplayName: "Claude Sonnet 4.6", Provider: "anthropic", Modes: []string{"thinking"}, ContextWindow: 200000}, {ID: "gpt-6-luna", Provider: "openai", Unstable: true, ManualOnly: true, APIModelID: "gpt-6-luna", ContextWindow: 200000}}}
	var list struct {
		Data []PoeModel `json:"data"`
	}
	if err := json.Unmarshal([]byte(`{"data":[{"id":"claude-sonnet-4.6","owned_by":"Anthropic","metadata":{"display_name":"Claude Sonnet 4.6"},"architecture":{"output_modalities":["text"]},"pricing":{"prompt":"0.000003","completion":"0.000015"}},{"id":"claude-sonnet-5","owned_by":"Anthropic","metadata":{"display_name":"Claude Sonnet 5"},"architecture":{"output_modalities":["text"]},"pricing":{"prompt":"0.000004","completion":"0.000020"}},{"id":"gpt-6-luna","owned_by":"OpenAI","architecture":{"output_modalities":["text"]},"pricing":{"prompt":"0.000001","completion":"0.000005"}}]}`), &list); err != nil {
		t.Fatal(err)
	}
	state, err := MergePoe(base, list.Data, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	old, _ := state.Catalog.FindModel("claude-sonnet-4.6")
	newer, ok := state.Catalog.FindModel("claude-sonnet-5")
	bot, _ := state.Catalog.FindModel("gpt-6-luna")
	if !old.Legacy || !ok || newer.Legacy || !newer.ManualOnly || len(newer.Modes) != 1 || newer.Modes[0] != "thinking" || bot.Unstable {
		t.Fatalf("old=%+v newer=%+v bot=%+v", old, newer, bot)
	}
}
