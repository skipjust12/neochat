package modelcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"neochat/router"
)

const PoeURL = "https://api.poe.com/v1/models"

type PoeModel struct {
	ID           string   `json:"id"`
	Description  string   `json:"description"`
	Owner        string   `json:"owned_by"`
	Created      int64    `json:"created"`
	Features     []string `json:"supported_features"`
	Endpoints    []string `json:"supported_endpoints"`
	Architecture struct {
		Input  []string `json:"input_modalities"`
		Output []string `json:"output_modalities"`
	} `json:"architecture"`
	Window struct {
		Context int `json:"context_length"`
		Output  int `json:"max_output_tokens"`
	} `json:"context_window"`
	Context  int `json:"context_length"`
	Metadata struct {
		Name string `json:"display_name"`
	} `json:"metadata"`
	Pricing *struct {
		Prompt     string  `json:"prompt"`
		Completion string  `json:"completion"`
		CacheRead  *string `json:"input_cache_read"`
		CacheWrite *string `json:"input_cache_write"`
	} `json:"pricing"`
	Parameters []struct {
		Name   string `json:"name"`
		Schema struct {
			Enum []json.RawMessage `json:"enum"`
		} `json:"schema"`
	} `json:"parameters"`
}

func NewPoe(base router.Catalog, options ...Option) *Live {
	options = append([]Option{func(l *Live) { l.poe = true; l.url = PoeURL; l.ttl = time.Hour }}, options...)
	return New(base, options...)
}

func (l *Live) fetchPoe(ctx context.Context, now time.Time) (*State, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Poe models: HTTP %d", resp.StatusCode)
	}
	var list struct {
		Data []PoeModel `json:"data"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&list); err != nil {
		return nil, err
	}
	return MergePoe(l.base, list.Data, now)
}

var poeOwners = map[string]string{"anthropic": "anthropic", "openai": "openai", "google": "google", "xai": "spacexai", "x-ai": "spacexai", "deepseek": "deepseek", "moonshotai": "moonshot", "moonshot ai": "moonshot"}

// Only spelling/slug aliases are normalized. Never map an unavailable model
// to a different model's name or charge the old vendor's prices for Poe.
func poeAlias(model router.Model) string {
	id := strings.ToLower(model.ResolveAPIModelID())
	if _, tail, ok := strings.Cut(id, "/"); ok {
		id = tail
	}
	switch model.ID {
	case "claude-haiku-4-5":
		return "claude-haiku-4.5"
	case "nano-banana-pro":
		return "nano-banana-pro"
	case "nano-banana-2-lite":
		return "nano-banana-2-lite"
	case "deepseek-v4.1-flash":
		return "deepseek-v4.1-flash-1017"
	case "deepseek-v4-flash":
		return "deepseek-v4-flash"
	}
	return strings.TrimSuffix(id, "-preview")
}
func poeRate(s string) (float64, bool) {
	n, err := strconv.ParseFloat(s, 64)
	return n * 1e6, err == nil && n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func applyPoe(m router.Model, p PoeModel) router.Model {
	m.Unavailable = false
	m.Unstable = false
	m.APIModelID = p.ID
	m.Poe = true
	m.PoeEndpoints = append([]string(nil), p.Endpoints...)
	m.PoeReasoningParameter = ""
	m.PoeReasoningValues = nil
	// Keep the curated rates only as internal estimates if Poe has no price.
	// A null price must never make a previously paid model appear free.
	m.CostCacheReadPerMTok = nil
	m.CostCacheWritePerMTok = nil
	if p.Metadata.Name != "" {
		m.DisplayName = p.Metadata.Name
	}
	m.Description = p.Description
	oldFile := contains(m.InputModalities, "file")
	m.InputModalities = nil
	for _, kind := range p.Architecture.Input {
		if kind == "text" || kind == "image" || kind == "video" || kind == "file" {
			m.InputModalities = append(m.InputModalities, kind)
		}
	}
	if oldFile && m.Kind == "" {
		m.InputModalities = append(m.InputModalities, "file")
	}
	if len(m.InputModalities) == 0 {
		m.InputModalities = []string{"text"}
	}
	m.ContextWindow = firstPositive(p.Window.Context, p.Context, m.ContextWindow)
	m.MaxOutputTokens = firstPositive(p.Window.Output, m.MaxOutputTokens)
	m.ToolCalling = contains(p.Features, "tools")
	m.SupportsTools = nil
	if m.ToolCalling {
		m.SupportsTools = []string{router.ToolWebSearch, router.ToolCodeExecution}
	}
	m.Reasoning = ""
	for _, param := range p.Parameters {
		switch param.Name {
		case "thinking_budget":
			m.Reasoning = "effort"
			m.PoeReasoningParameter = "thinking_budget"
		case "reasoning_effort", "output_effort", "thinking_level", "effort":
			m.PoeReasoningParameter = param.Name
			for _, raw := range param.Schema.Enum {
				var v string
				if json.Unmarshal(raw, &v) == nil {
					m.PoeReasoningValues = append(m.PoeReasoningValues, v)
				}
			}
			m.Reasoning = "effort"
			if contains(p.Endpoints, "/v1/messages") {
				m.Reasoning = "adaptive"
			}
		}
	}
	if m.Kind == router.KindImage {
		m.CostPerImageUSD = 0.5
		// Some Poe entries say text-only while explicitly advertising image
		// editing. Preserve the curated reference workflow; refusals are surfaced.
		if m.MaxReferenceImages > 0 && !contains(m.InputModalities, "image") {
			m.InputModalities = append(m.InputModalities, "image")
		}
	}
	if m.Kind == router.KindVideo {
		m.CostPerVideoUSD = 5
		m.Video = poeVideoOptions(p)
		m.MaxReferenceImages = 0
		if contains(p.Architecture.Input, "image") {
			m.MaxReferenceImages = 3
		}
		if contains(p.Endpoints, "/v1/videos") {
			m.MaxReferenceImages = 1
		}
	}
	if p.Pricing != nil {
		if p.Pricing.CacheRead != nil {
			if n, ok := poeRate(*p.Pricing.CacheRead); ok {
				m.CostCacheReadPerMTok = &n
			}
		}
		if p.Pricing.CacheWrite != nil {
			if n, ok := poeRate(*p.Pricing.CacheWrite); ok {
				m.CostCacheWritePerMTok = &n
			}
		}
		if n, ok := poeRate(p.Pricing.Prompt); ok {
			m.CostInputPerMTok = n
		}
		if n, ok := poeRate(p.Pricing.Completion); ok {
			m.CostOutputPerMTok = n
		}
	}
	return m
}

func MergePoe(base router.Catalog, live []PoeModel, now time.Time) (*State, error) {
	if len(live) == 0 {
		return nil, fmt.Errorf("Poe returned an empty model list")
	}
	byID := map[string]PoeModel{}
	for _, p := range live {
		byID[strings.ToLower(p.ID)] = p
	}
	state := &State{UpdatedAt: now, Added: map[string]time.Time{}, Retired: map[string]bool{}}
	known := map[string]bool{}
	for _, m := range base.Models {
		id := poeAlias(m)
		p, ok := byID[id]
		if !ok {
			// Poe accepts public bot names, including bots omitted from /v1/models.
			// Keep these as explicit manual choices; do not infer nonexistence.
			m.ManualOnly = true
			m.Poe = true
			m.Unavailable = false
			m.Unstable = true
			m.APIModelID = m.ID
			m.PoeEndpoints = []string{"/v1/chat/completions"}
			m.PoeReasoningParameter = ""
			m.PoeReasoningValues = nil
			m.CostCacheReadPerMTok = nil
			m.CostCacheWritePerMTok = nil
			if m.Kind == router.KindImage {
				m.CostPerImageUSD = 0.5
			}
			if m.Kind == router.KindVideo {
				m.CostPerVideoUSD = 5
				if m.Video != nil {
					v := *m.Video
					v.Prices = nil
					m.Video = &v
				}
			}
			state.Catalog.Models = append(state.Catalog.Models, m)
			continue
		}
		known[id] = true
		state.Catalog.Models = append(state.Catalog.Models, applyPoe(m, p))
	}
	for _, p := range live {
		if known[strings.ToLower(p.ID)] || !slugPattern.MatchString(strings.ToLower(p.ID)) {
			continue
		}
		owner := poeOwners[strings.ToLower(p.Owner)]
		if owner == "" {
			continue
		}
		var template *router.Model
		sameLine := false
		name := p.Metadata.Name
		if name == "" {
			name = p.ID
		}
		line, _ := lineOf(name)
		for i := range base.Models {
			m := &base.Models[i]
			kind := ""
			if !contains(p.Architecture.Output, "text") {
				if contains(p.Architecture.Output, "image") {
					kind = router.KindImage
				}
				if contains(p.Architecture.Output, "video") {
					kind = router.KindVideo
				}
			}
			if len(p.Architecture.Output) == 0 || (kind == "" && !contains(p.Architecture.Output, "text")) {
				continue
			}
			if m.Provider == owner && m.Kind == kind {
				if template == nil {
					template = m
				}
				if currentLine, _ := lineOf(nameOf(*m)); currentLine == line {
					template = m
					sameLine = true
					break
				}
			}
		}
		if template == nil {
			continue
		}
		m := *template
		m.ID = strings.ToLower(p.ID)
		m.Legacy = false
		m.ManualOnly = true
		m = applyPoe(m, p)
		if !sameLine {
			m.Modes = []string{tierByPrice(m)}
		}
		state.Catalog.Models = append(state.Catalog.Models, m)
		state.Added[m.ID] = time.UnixMilli(p.Created)
	}
	// Keep older versions in Legacy when a listed model continues their line.
	// Unlisted bots stay where the curated catalog put them, with Unstable.
	for i, m := range state.Catalog.Models {
		if m.Unstable {
			continue
		}
		line, version := lineOf(nameOf(m))
		if len(version) == 0 {
			continue
		}
		for _, newer := range state.Catalog.Models {
			if newer.Unstable || newer.Provider != m.Provider || newer.Kind != m.Kind {
				continue
			}
			otherLine, otherVersion := lineOf(nameOf(newer))
			if line == otherLine && compareVersions(otherVersion, version) > 0 {
				state.Catalog.Models[i].Legacy = true
				break
			}
		}
	}
	matched := len(known)
	if matched == 0 {
		return nil, fmt.Errorf("Poe catalog has no recognized curated models")
	}
	return state, nil
}

func poeVideoOptions(p PoeModel) *router.VideoOptions {
	v := &router.VideoOptions{Durations: []string{"auto"}, Resolutions: []string{"720p"}, AspectRatios: []string{"16:9"}}
	for _, param := range p.Parameters {
		values := []string{}
		for _, raw := range param.Schema.Enum {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				values = append(values, s)
			}
		}
		if len(values) == 0 {
			continue
		}
		switch param.Name {
		case "duration":
			v.Durations = values
		case "resolution":
			v.Resolutions = values
		case "aspect_ratio":
			v.AspectRatios = values
		}
	}
	if contains(p.Architecture.Input, "video") {
		v.MaxReferenceVideos = 1
	}
	return v
}

// LoadPoe starts from a checked-in public metadata snapshot, so network failure
// at startup never sends a vendor-prefixed model slug to Poe.
func LoadPoe(base router.Catalog, path string) (router.Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return router.Catalog{}, err
	}
	var list struct {
		Data []PoeModel `json:"data"`
	}
	if err = json.Unmarshal(data, &list); err != nil {
		return router.Catalog{}, err
	}
	state, err := MergePoe(base, list.Data, time.Now())
	if err != nil {
		return router.Catalog{}, err
	}
	return state.Catalog, nil
}
