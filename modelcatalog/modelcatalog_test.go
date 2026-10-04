package modelcatalog

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"neochat/router"
)

var released = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix()

func curated() router.Catalog {
	return router.Catalog{Models: []router.Model{
		{ID: "claude-opus-5.5", APIModelID: "anthropic/claude-opus-5.5", DisplayName: "Claude Opus 5.5", Description: "For complex tasks", Provider: "anthropic", Modes: []string{"max"}},
		{ID: "claude-haiku-4-5", APIModelID: "anthropic/claude-haiku-4.5", DisplayName: "Claude Haiku 4.5", Description: "Fast and quick responses", Provider: "anthropic", Modes: []string{"instant"}, TaskCategoryScores: map[string]float64{"general": 0.8}},
		{ID: "claude-sonnet-4.6", APIModelID: "anthropic/claude-sonnet-4.6", DisplayName: "Claude Sonnet 4.6", Provider: "anthropic", Modes: []string{"thinking"}, Legacy: true},
		{ID: "gpt-6.1-sol", APIModelID: "openai/gpt-6.1-sol", DisplayName: "GPT-6.1 Sol", Description: "Powerful reasoning model", Provider: "openai", Modes: []string{"thinking"}},
		{ID: "gpt-6-sol", APIModelID: "openai/gpt-6-sol", DisplayName: "GPT-6 Sol", Provider: "openai", Modes: []string{"thinking"}, Legacy: true},
		{ID: "deepseek-v4-pro-0813", APIModelID: "deepseek/deepseek-v4-pro-0813", DisplayName: "DeepSeek V4 Pro", Description: "Flagship", Provider: "deepseek", Modes: []string{"thinking"}},
		{ID: "nano-banana-pro", APIModelID: "google/gemini-3-pro-image-preview", DisplayName: "Nano Banana Pro", Description: "Most advanced image model", Provider: "google", Kind: router.KindImage, CostPerImageUSD: 0.11, Modes: []string{"max"}},
	}}
}

func chat(id, name string, created int64, outRUB string, params ...string) LiveModel {
	var m LiveModel
	m.ID, m.Name, m.Type, m.Created = id, name, "chat", created
	m.Endpoints = []string{"/api/v1/chat/completions"}
	m.ContextLength, m.MaxCompletionTokens = 400000, 64000
	m.Architecture.InputModalities = []string{"text", "image", "file", "audio"}
	m.Architecture.OutputModalities = []string{"text"}
	m.TopProvider = &struct {
		ContextLength       int      `json:"context_length"`
		MaxCompletionTokens int      `json:"max_completion_tokens"`
		SupportedParameters []string `json:"supported_parameters"`
		Pricing             struct {
			Currency             string `json:"currency"`
			PromptPerMillion     string `json:"prompt_per_million"`
			CompletionPerMillion string `json:"completion_per_million"`
			PerRequest           string `json:"per_request"`
			Tiers                []struct {
				Conditions []string `json:"conditions"`
				CostRUB    string   `json:"cost_rub"`
			} `json:"tiers"`
		} `json:"pricing"`
	}{SupportedParameters: params}
	m.TopProvider.Pricing.Currency = "RUB"
	m.TopProvider.Pricing.PromptPerMillion = "117.068"
	m.TopProvider.Pricing.CompletionPerMillion = outRUB
	return m
}

// polzaToday is Polza's list as of the curated catalog: every curated
// model, plus older ones the curation left out.
func polzaToday() []LiveModel {
	var live []LiveModel
	for i, m := range curated().Models {
		entry := chat(m.ResolveAPIModelID(), m.DisplayName, released-int64(i)*86400, "585.34")
		if m.Kind == router.KindImage {
			entry.Type, entry.Endpoints = "image", []string{"/api/v1/media"}
			entry.TopProvider.Pricing.PerRequest = "13.5"
		}
		live = append(live, entry)
	}
	return append(live, chat("openai/gpt-4o", "OpenAI: GPT-4o", released-300*86400, "1000"))
}

func ids(models []router.Model, legacy bool, provider string) []string {
	var out []string
	for _, m := range models {
		if m.Legacy == legacy && m.Provider == provider {
			out = append(out, m.ID)
		}
	}
	return out
}

func find(t *testing.T, s *State, id string) router.Model {
	t.Helper()
	m, ok := s.Catalog.FindModel(id)
	if !ok {
		t.Fatalf("%s not in the merged catalog", id)
	}
	return m
}

func TestMerge_NothingNew(t *testing.T) {
	s, err := Merge(curated(), polzaToday(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Added) != 0 || len(s.Retired) != 0 || len(s.Catalog.Models) != len(curated().Models) {
		t.Fatalf("added %v, retired %v, %d models", s.Added, s.Retired, len(s.Catalog.Models))
	}
}

func TestMerge_NewVersionTakesTheLinesPlace(t *testing.T) {
	live := append(polzaToday(),
		chat("anthropic/claude-haiku-5.5", "Anthropic: Claude Haiku 5.5", released+86400, "585.34", "tools", "reasoning"),
		chat("openai/gpt-6.2-sol", "OpenAI: GPT-6.2 Sol", released+2*86400, "585.34", "tools", "reasoning_effort"),
	)
	s, err := Merge(curated(), live, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(s.Catalog.Models, false, "anthropic"), ","); got != "claude-opus-5.5,claude-haiku-5.5" {
		t.Fatalf("current Anthropic models = %s", got)
	}
	if got := strings.Join(ids(s.Catalog.Models, true, "anthropic"), ","); got != "claude-haiku-4-5,claude-sonnet-4.6" {
		t.Fatalf("legacy Anthropic models = %s (the replaced one goes first)", got)
	}
	if got := strings.Join(ids(s.Catalog.Models, true, "openai"), ","); got != "gpt-6.1-sol,gpt-6-sol" {
		t.Fatalf("legacy OpenAI models = %s", got)
	}
	haiku := find(t, s, "claude-haiku-5.5")
	if haiku.DisplayName != "Claude Haiku 5.5" || haiku.APIModelID != "anthropic/claude-haiku-5.5" || haiku.Description != "Fast and quick responses" ||
		haiku.Modes[0] != "instant" || !haiku.ManualOnly || haiku.Reasoning != "adaptive" || !haiku.ToolCalling ||
		strings.Join(haiku.InputModalities, ",") != "text,image,file" || haiku.CostInputPerMTok != 1 || haiku.CostOutputPerMTok != 5 ||
		haiku.ContextWindow != 400000 || haiku.MaxOutputTokens != 64000 || haiku.TaskCategoryScores["general"] != 0.8 {
		t.Fatalf("Haiku 5.5 = %+v", haiku)
	}
	if sol := find(t, s, "gpt-6.2-sol"); sol.Reasoning != "effort" || sol.Modes[0] != "thinking" {
		t.Fatalf("GPT-6.2 Sol = %+v", sol)
	}
	if !s.Added["claude-haiku-5.5"].Equal(time.Unix(released+86400, 0)) || len(s.Added) != 2 {
		t.Fatalf("added = %v", s.Added)
	}
	if find(t, s, "claude-haiku-4-5").Description == "" {
		t.Fatal("the replaced model lost its data")
	}
}

func TestMerge_NewLineAndImageModel(t *testing.T) {
	grok := chat("x-ai/grok-5", "SpaceXAI: Grok 5", released+86400, "2000")
	image := chat("google/gemini-4-pro-image", "Google: Nano Banana 3 Pro (Gemini 4 Pro Image)", released+86400, "0")
	image.Type, image.Endpoints = "image", []string{"/api/v1/media"}
	image.TopProvider.Pricing.Tiers = append(image.TopProvider.Pricing.Tiers, struct {
		Conditions []string `json:"conditions"`
		CostRUB    string   `json:"cost_rub"`
	}{CostRUB: "15"})
	image.Parameters.Images = &struct {
		Max int `json:"max"`
	}{Max: 10}
	deepseek := chat("deepseek/deepseek-v5", "DeepSeek: DeepSeek V5", released+86400, "100")
	s, err := Merge(curated(), append(polzaToday(), grok, image, deepseek), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if g := find(t, s, "grok-5"); g.Legacy || g.Provider != "spacexai" || g.Modes[0] != "max" || g.Description != "" {
		t.Fatalf("Grok 5 = %+v (a new line, priced like max)", g)
	}
	if got := strings.Join(ids(s.Catalog.Models, false, "deepseek"), ","); got != "deepseek-v5,deepseek-v4-pro-0813" {
		t.Fatalf("DeepSeek = %s: a new line goes first, the V4 Pro line stays", got)
	}
	banana := find(t, s, "gemini-4-pro-image")
	if banana.Kind != router.KindImage || banana.DisplayName != "Nano Banana 3 Pro" || banana.MaxReferenceImages != 10 || banana.CostPerImageUSD != 0.1281 || banana.Description != "Most advanced image model" {
		t.Fatalf("image model = %+v", banana)
	}
	if !find(t, s, "nano-banana-pro").Legacy {
		t.Fatal("Nano Banana Pro should be legacy now")
	}
}

func TestMerge_SkipsVariantsOldVersionsAndOtherVendors(t *testing.T) {
	live := append(polzaToday(),
		chat("openai/gpt-6.2-sol-search", "OpenAI: GPT-6.2 Sol Search", released+86400, "500"),
		chat("deepseek/deepseek-v4.2-flash-exp", "DeepSeek: V4.2 Flash Exp", released+86400, "10"),
		chat("openai/gpt-5.9-sol", "OpenAI: GPT-5.9 Sol", released+86400, "500"), // older than 6.1
		chat("qwen/qwen-4", "Qwen: Qwen 4", released+86400, "10"),
		chat(`openai/gpt-7"><img src=x>`, "OpenAI: GPT-7", released+86400, "500"),
		chat("openai/GPT-7", "OpenAI: GPT-7", released+86400, "500"),
	)
	s, err := Merge(curated(), live, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Added) != 0 {
		t.Fatalf("added %v, want nothing", s.Added)
	}
}

func TestMerge_RetiredAndBadLists(t *testing.T) {
	live := polzaToday()[1:] // Opus 5.5 is gone
	s, err := Merge(curated(), live, time.Now())
	if err != nil || !s.Retired["claude-opus-5.5"] || len(s.Retired) != 1 {
		t.Fatalf("retired = %v, %v", s.Retired, err)
	}
	if _, err := Merge(curated(), polzaToday()[5:], time.Now()); err == nil {
		t.Fatal("a list missing most curated models was merged")
	}
	flood := polzaToday()
	for i := 0; i < maxAdded+1; i++ {
		flood = append(flood, chat(fmt.Sprintf("openai/gpt-x%d", i), fmt.Sprintf("GPT X%d", i), released+86400, "10"))
	}
	if _, err := Merge(curated(), flood, time.Now()); err == nil {
		t.Fatal("an implausible number of new models was merged")
	}
}

func TestLineOf(t *testing.T) {
	for name, want := range map[string]string{
		"GPT-6.1 Sol Pro":        "gpt sol pro 6.1",
		"DeepSeek V4 Flash 0731": "deepseek v flash 4",
		"DeepSeek R1 (0528)":     "deepseek r 1",
		"Gemma 4 26B A4B":        "gemma 26b a4b 4",
		"Gemini 3.1 Pro Preview": "gemini pro 3.1",
		"Gemini 3.5 Flash-Lite":  "gemini flash lite 3.5",
		"Kimi K2.7 Code":         "kimi k code 2.7",
		"Nano Banana Pro":        "nano banana pro ",
	} {
		line, version := lineOf(name)
		parts := make([]string, len(version))
		for i, v := range version {
			parts[i] = fmt.Sprint(v)
		}
		if got := line + " " + strings.Join(parts, "."); got != want {
			t.Errorf("lineOf(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestRefresh_TTLCooldownAndForce(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		models := polzaToday()
		if calls.Load() > 1 {
			models = append(models, chat("anthropic/claude-haiku-5.5", "Anthropic: Claude Haiku 5.5", released+86400, "585.34"))
		}
		body := `{"data":[`
		for i, m := range models {
			if i > 0 {
				body += ","
			}
			body += fmt.Sprintf(`{"id":%q,"name":%q,"type":%q,"created":%d,"endpoints":["/api/v1/chat/completions","/api/v1/media"],"architecture":{"input_modalities":["text"],"output_modalities":["text"]},"top_provider":{"pricing":{"currency":"RUB","prompt_per_million":"1","completion_per_million":"1","per_request":"5"}}}`, m.ID, m.Name, m.Type, m.Created)
		}
		fmt.Fprint(w, body+`]}`)
	}))
	defer vendor.Close()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	live := New(curated(), WithURL(vendor.URL), WithTiming(10*time.Minute, 30*time.Second), WithClock(clock))
	ctx := context.Background()

	if !live.Current().UpdatedAt.IsZero() || len(live.Current().Catalog.Models) != len(curated().Models) {
		t.Fatal("a new Live should serve the curated catalog")
	}
	if _, err := live.Refresh(ctx, false); err != nil || calls.Load() != 1 || !live.Current().UpdatedAt.Equal(now) {
		t.Fatalf("first refresh: %v, %d calls", err, calls.Load())
	}
	now = now.Add(5 * time.Minute)
	live.Refresh(ctx, false)
	if calls.Load() != 1 {
		t.Fatal("a fresh list was fetched again")
	}
	live.Refresh(ctx, true)
	if calls.Load() != 2 {
		t.Fatal("force didn't fetch")
	}
	if _, ok := live.Current().Catalog.FindModel("claude-haiku-5.5"); !ok {
		t.Fatal("the new model didn't arrive")
	}
	now = now.Add(10 * time.Second)
	live.Refresh(ctx, true)
	if calls.Load() != 2 {
		t.Fatal("force inside the cooldown fetched")
	}

	fail.Store(true)
	now = now.Add(11 * time.Minute)
	if _, err := live.Refresh(ctx, false); err == nil || calls.Load() != 3 {
		t.Fatalf("stale refresh against a failing vendor: %v, %d calls", err, calls.Load())
	}
	if _, ok := live.Current().Catalog.FindModel("claude-haiku-5.5"); !ok {
		t.Fatal("a failed refresh dropped the last good list")
	}
	now = now.Add(time.Second)
	if _, err := live.Refresh(ctx, false); err == nil || calls.Load() != 3 {
		t.Fatalf("retry inside the cooldown: %v, %d calls", err, calls.Load())
	}
}
