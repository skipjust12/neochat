// Package modelcatalog keeps the model catalog current. configs/models.json
// is the curated list; Polza's public catalog (GET /api/v1/models, no key
// needed) says what has come out since. A model Polza added after the
// newest curated one, from a vendor the picker shows, joins the catalog:
// one that continues a known line (Claude Haiku 5.5 after Claude Haiku
// 4.5) takes that line's place, and the model it replaces moves to Legacy;
// one that starts a new line is added at the top of its vendor's list.
// Curated models Polza no longer lists are reported as retired, for the
// picker to hide.
//
// Added models are Manual-only: the router's task scores were never
// calibrated for them. They take the replaced model's tier, description
// and routing data, and everything else (prices, context window, inputs,
// tools, reasoning) from Polza's own entry.
package modelcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"neochat/router"
)

const (
	// DefaultURL is Polza's public model list.
	DefaultURL = "https://polza.ai/api/v1/models"

	// RUBPerUSD is the rate the curated prices were converted from Polza's
	// rubles at (docs/running-locally.md); added models use the same.
	RUBPerUSD = 117.068

	// DefaultTTL is how old the list may get before a visit refreshes it.
	DefaultTTL = 10 * time.Minute

	// DefaultCooldown is the least time between two fetches, forced or
	// not, however many people press Refresh.
	DefaultCooldown = 30 * time.Second

	// maxAdded bounds one merge: more new models than this means the
	// cutoff went wrong (Polza re-dated its list, say), not a release wave,
	// and the merge is refused.
	maxAdded = 30

	maxBodyBytes = 16 << 20
	fetchTimeout = 20 * time.Second
)

// vendors maps Polza's vendor prefix to the catalog provider, for the
// vendors the picker has a page (and a logo) for.
var vendors = map[string]string{
	"anthropic":  "anthropic",
	"openai":     "openai",
	"x-ai":       "spacexai",
	"deepseek":   "deepseek",
	"google":     "google",
	"moonshotai": "moonshot",
}

// variantMarkers in a Polza id or name mark a mode of a model rather than
// a model of its own; those are left out.
var variantMarkers = []string{"-exp", "experimental", "customtools", "safeguard", "multi-agent", "deep-research", "realtime", "audio", "search", "-tts", "embedding"}

// State is one merged catalog.
type State struct {
	Catalog router.Catalog
	// UpdatedAt is when Polza's list was last fetched; zero until the
	// first fetch succeeds (the curated list alone until then).
	UpdatedAt time.Time
	// Added holds the models this merge added, by ID, with when Polza
	// released them.
	Added map[string]time.Time
	// Retired holds curated models Polza no longer lists, by ID.
	Retired map[string]bool
}

// Live is the catalog the server answers from: the curated one merged with
// Polza's list as of the last refresh. Safe for concurrent use.
type Live struct {
	base     router.Catalog
	url      string
	client   *http.Client
	ttl      time.Duration
	cooldown time.Duration
	now      func() time.Time

	sem     chan struct{} // one refresh at a time
	lastTry time.Time
	lastErr error
	state   atomic.Pointer[State]
}

// Option configures a Live.
type Option func(*Live)

// WithURL points Live at another copy of Polza's list (tests).
func WithURL(url string) Option { return func(l *Live) { l.url = url } }

// WithTiming sets the refresh TTL and cooldown.
func WithTiming(ttl, cooldown time.Duration) Option {
	return func(l *Live) { l.ttl, l.cooldown = ttl, cooldown }
}

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option { return func(l *Live) { l.now = now } }

// New starts from the curated catalog alone; Refresh brings in Polza's.
func New(base router.Catalog, options ...Option) *Live {
	l := &Live{
		base:     base,
		url:      DefaultURL,
		client:   &http.Client{Timeout: fetchTimeout},
		ttl:      DefaultTTL,
		cooldown: DefaultCooldown,
		now:      time.Now,
		sem:      make(chan struct{}, 1),
	}
	for _, option := range options {
		option(l)
	}
	l.state.Store(&State{Catalog: base, Added: map[string]time.Time{}, Retired: map[string]bool{}})
	return l
}

// Current is the catalog as of the last refresh.
func (l *Live) Current() *State { return l.state.Load() }

// Refresh fetches Polza's list again when the current one is older than
// the TTL, or always when force is set -- but never twice within the
// cooldown: inside it, the current state (and the last fetch's error, if
// it failed) is returned. A fetch already under way is waited for rather
// than repeated. On failure the current state stays.
func (l *Live) Refresh(ctx context.Context, force bool) (*State, error) {
	if !force && l.fresh() {
		return l.Current(), nil
	}
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		return l.Current(), ctx.Err()
	}
	defer func() { <-l.sem }()
	if !force && l.fresh() {
		return l.Current(), nil
	}
	now := l.now()
	if !l.lastTry.IsZero() && now.Sub(l.lastTry) < l.cooldown {
		return l.Current(), l.lastErr
	}
	l.lastTry = now
	state, err := l.fetchAndMerge(ctx, now)
	l.lastErr = err
	if err != nil {
		return l.Current(), err
	}
	l.state.Store(state)
	return state, nil
}

func (l *Live) fresh() bool {
	updated := l.Current().UpdatedAt
	return !updated.IsZero() && l.now().Sub(updated) < l.ttl
}

func (l *Live) fetchAndMerge(ctx context.Context, now time.Time) (*State, error) {
	// The fetch outlives a caller that stops waiting: the next one gets
	// its result instead of starting over.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, l.url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := l.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("modelcatalog: fetch Polza's model list: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("modelcatalog: Polza's model list answered HTTP %d", response.StatusCode)
	}
	var list struct {
		Data []LiveModel `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxBodyBytes)).Decode(&list); err != nil {
		return nil, fmt.Errorf("modelcatalog: parse Polza's model list: %w", err)
	}
	return Merge(l.base, list.Data, now)
}

// LiveModel is one entry of Polza's list, the fields this package reads.
type LiveModel struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Type                string   `json:"type"`
	Created             int64    `json:"created"`
	Endpoints           []string `json:"endpoints"`
	ContextLength       int      `json:"context_length"`
	MaxCompletionTokens int      `json:"max_completion_tokens"`
	Architecture        struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
	TopProvider *struct {
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
	} `json:"top_provider"`
	Parameters struct {
		Images *struct {
			Max int `json:"max"`
		} `json:"images"`
	} `json:"parameters"`
}

// Merge brings the models Polza released after the newest curated one into
// the curated catalog (see the package comment). It refuses a list that
// looks wrong -- one missing most curated models, or with implausibly many
// new ones -- rather than reshape the picker on bad data.
func Merge(base router.Catalog, live []LiveModel, now time.Time) (*State, error) {
	byID := make(map[string]LiveModel, len(live))
	for _, m := range live {
		byID[m.ID] = m
	}
	var cutoff int64
	matched := 0
	retired := map[string]bool{}
	known := map[string]bool{}
	for _, m := range base.Models {
		known[m.ResolveAPIModelID()] = true
		entry, ok := byID[m.ResolveAPIModelID()]
		if !ok {
			retired[m.ID] = true
			continue
		}
		matched++
		cutoff = max(cutoff, entry.Created)
	}
	if matched == 0 || matched*2 < len(base.Models) {
		return nil, fmt.Errorf("modelcatalog: Polza's list has only %d of %d curated models", matched, len(base.Models))
	}

	var candidates []LiveModel
	for _, m := range live {
		if m.Created > cutoff && !known[m.ID] && wanted(m) {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) > maxAdded {
		return nil, fmt.Errorf("modelcatalog: %d models newer than the curated catalog, more than %d at once", len(candidates), maxAdded)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Created != candidates[j].Created {
			return candidates[i].Created < candidates[j].Created
		}
		return candidates[i].ID < candidates[j].ID
	})

	models := append([]router.Model(nil), base.Models...)
	added := map[string]time.Time{}
	for _, c := range candidates {
		model, ok := fromLive(c)
		if !ok || hasID(models, model.ID) {
			continue
		}
		line, version := lineOf(model.DisplayName)
		var members, heads []int
		newest := []int(nil)
		for i, m := range models {
			if m.Provider != model.Provider || m.Kind != model.Kind {
				continue
			}
			if l, v := lineOf(nameOf(m)); l == line {
				members = append(members, i)
				if newest == nil || compareVersions(v, newest) > 0 {
					newest = v
				}
				if !m.Legacy {
					heads = append(heads, i)
				}
			}
		}
		if len(members) > 0 && compareVersions(version, newest) < 0 {
			continue // an older version Polza listed late
		}
		if len(heads) > 0 {
			inherit(&model, models[heads[0]])
		} else {
			model.Modes = []string{tierByPrice(model)}
		}
		models = place(models, model, heads)
		added[model.ID] = time.Unix(c.Created, 0)
	}
	return &State{Catalog: router.Catalog{Models: models}, UpdatedAt: now, Added: added, Retired: retired}, nil
}

// slugPattern is what an added model's ID may look like: it ends up in
// the page's markup and selectors, so nothing that could break out of an
// attribute value.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,79}$`)

// maxNameRunes bounds an added model's display name.
const maxNameRunes = 60

// wanted reports whether a Polza entry is a chat or image model from a
// vendor the picker shows, and not a variant of another model.
func wanted(m LiveModel) bool {
	vendor, slug, ok := strings.Cut(m.ID, "/")
	if !ok || vendors[vendor] == "" || !slugPattern.MatchString(slug) {
		return false
	}
	lower := strings.ToLower(m.ID + " " + m.Name)
	for _, marker := range variantMarkers {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	switch m.Type {
	case "chat":
		return contains(m.Endpoints, "/api/v1/chat/completions") && contains(m.Architecture.OutputModalities, "text")
	case "image":
		return contains(m.Endpoints, "/api/v1/media")
	}
	return false
}

// fromLive builds a catalog entry from Polza's; false when its price can't
// be read.
func fromLive(c LiveModel) (router.Model, bool) {
	vendor, slug, _ := strings.Cut(c.ID, "/")
	if c.TopProvider == nil || !strings.EqualFold(c.TopProvider.Pricing.Currency, "RUB") {
		return router.Model{}, false
	}
	tp := c.TopProvider
	m := router.Model{
		ID:          slug,
		APIModelID:  c.ID,
		DisplayName: displayName(c.Name),
		Provider:    vendors[vendor],
		ManualOnly:  true,
	}
	if m.DisplayName == "" || utf8.RuneCountInString(m.DisplayName) > maxNameRunes {
		m.DisplayName = slug
	}
	if c.Type == "image" {
		price, ok := imagePrice(c)
		if !ok {
			return router.Model{}, false
		}
		m.Kind = router.KindImage
		m.CostPerImageUSD = round4(price / RUBPerUSD)
		if c.Parameters.Images != nil {
			m.MaxReferenceImages = c.Parameters.Images.Max
		}
		m.InputModalities = []string{"text"}
		if m.MaxReferenceImages > 0 {
			m.InputModalities = append(m.InputModalities, "image")
		}
		m.ContextWindow = firstPositive(c.ContextLength, tp.ContextLength)
		m.SupportsModality = []string{"text", "image"}
		m.SupportsTools = []string{}
		m.SupportsOutputFormats = []string{"image"}
		return m, true
	}
	in, errIn := strconv.ParseFloat(tp.Pricing.PromptPerMillion, 64)
	out, errOut := strconv.ParseFloat(tp.Pricing.CompletionPerMillion, 64)
	if errIn != nil || errOut != nil || in < 0 || out < 0 {
		return router.Model{}, false
	}
	m.CostInputPerMTok = round4(in / RUBPerUSD)
	m.CostOutputPerMTok = round4(out / RUBPerUSD)
	m.ContextWindow = firstPositive(c.ContextLength, tp.ContextLength)
	m.MaxOutputTokens = firstPositive(c.MaxCompletionTokens, tp.MaxCompletionTokens)
	for _, kind := range []string{"text", "image", "file"} {
		if contains(c.Architecture.InputModalities, kind) {
			m.InputModalities = append(m.InputModalities, kind)
		}
	}
	if len(m.InputModalities) == 0 {
		m.InputModalities = []string{"text"}
	}
	m.ToolCalling = contains(tp.SupportedParameters, "tools")
	if contains(tp.SupportedParameters, "reasoning") || contains(tp.SupportedParameters, "reasoning_effort") {
		m.Reasoning = "effort"
		if m.Provider == "anthropic" {
			m.Reasoning = "adaptive"
		}
	}
	m.SupportsModality = []string{"text", "code"}
	if contains(m.InputModalities, "image") {
		m.SupportsModality = append(m.SupportsModality, "image")
	}
	m.SupportsTools = []string{}
	m.SupportsOutputFormats = []string{"text", "markdown", "json"}
	if m.ToolCalling {
		m.SupportsOutputFormats = append(m.SupportsOutputFormats, "function_call")
	}
	return m, true
}

// imagePrice is what one picture costs in rubles: the flat price, or the
// tier with no conditions (the default settings this server uses).
func imagePrice(c LiveModel) (float64, bool) {
	pricing := c.TopProvider.Pricing
	if price, err := strconv.ParseFloat(pricing.PerRequest, 64); err == nil && price > 0 {
		return price, true
	}
	for _, tier := range pricing.Tiers {
		if len(tier.Conditions) == 0 {
			if price, err := strconv.ParseFloat(tier.CostRUB, 64); err == nil && price > 0 {
				return price, true
			}
		}
	}
	return 0, false
}

// inherit gives a model the place-holding data of the one it replaces:
// tier (which also decides its spend pool), blurb and routing data.
func inherit(model *router.Model, from router.Model) {
	model.Modes = append([]string(nil), from.Modes...)
	model.Description = from.Description
	model.TaskCategoryScores = from.TaskCategoryScores
	model.TaskIntentScores = from.TaskIntentScores
	if len(from.SupportsTools) > 0 {
		model.SupportsTools = from.SupportsTools
	}
}

// tierByPrice guesses a new line's tier from what it costs, by the
// curated tiers' price bands.
func tierByPrice(m router.Model) string {
	if m.Kind == router.KindImage {
		if m.CostPerImageUSD >= 0.06 {
			return "max"
		}
		return "instant"
	}
	switch {
	case m.CostOutputPerMTok >= 15:
		return "max"
	case m.CostOutputPerMTok >= 3:
		return "thinking"
	}
	return "instant"
}

// place puts a new model where the line's current models were (or first
// among its vendor's models, for a new line), and moves those to the top
// of the vendor's legacy models.
func place(models []router.Model, model router.Model, heads []int) []router.Model {
	if len(heads) == 0 {
		at := len(models)
		for i, m := range models {
			if m.Provider == model.Provider {
				at = i
				break
			}
		}
		return insert(models, at, model)
	}
	replaced := map[int]bool{}
	var demoted []router.Model
	for _, i := range heads {
		replaced[i] = true
		old := models[i]
		old.Legacy = true
		demoted = append(demoted, old)
	}
	out := make([]router.Model, 0, len(models)+1)
	for i, m := range models {
		switch {
		case i == heads[0]:
			out = append(out, model)
		case !replaced[i]:
			out = append(out, m)
		}
	}
	at := len(out)
	for i, m := range out {
		if m.Provider == model.Provider && m.Legacy {
			at = i
			break
		}
	}
	return insert(out, at, demoted...)
}

func insert(models []router.Model, at int, more ...router.Model) []router.Model {
	out := make([]router.Model, 0, len(models)+len(more))
	out = append(out, models[:at]...)
	out = append(out, more...)
	return append(out, models[at:]...)
}

// nameOf is what a curated model's line is read from: its display name,
// else Polza's slug ("claude-haiku-4.5" keeps the version's dot, where the
// catalog ID "claude-haiku-4-5" may not).
func nameOf(m router.Model) string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	if _, slug, ok := strings.Cut(m.APIModelID, "/"); ok {
		return slug
	}
	return m.ID
}

var parenthesized = regexp.MustCompile(`\s*\([^)]*\)`)

// displayName is Polza's name without its "Vendor: " prefix and
// parenthesized notes: "Anthropic: Claude Haiku 5.5" -> "Claude Haiku 5.5".
func displayName(name string) string {
	if vendor, rest, ok := strings.Cut(name, ": "); ok && !strings.Contains(vendor, " ") {
		name = rest
	}
	return strings.TrimSpace(parenthesized.ReplaceAllString(name, ""))
}

var versionToken = regexp.MustCompile(`^([a-z]?)(\d+(?:\.\d+)*)$`)

// lineOf splits a model name into its line and version: "GPT-6.1 Sol Pro"
// is line "gpt sol pro", version 6.1; "DeepSeek V4 Flash 0731" is
// "deepseek v flash", 4; "Gemma 4 31B" is "gemma 31b", 4. Snapshot dates
// and words like "preview" don't change the line.
func lineOf(name string) (string, []int) {
	name = parenthesized.ReplaceAllString(strings.ToLower(name), "")
	tokens := strings.FieldsFunc(name, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.'
	})
	var line []string
	var version []int
	found := false
	for _, token := range tokens {
		token = strings.Trim(token, ".")
		if token == "" {
			continue
		}
		if !found {
			if match := versionToken.FindStringSubmatch(token); match != nil {
				found = true
				for _, part := range strings.Split(match[2], ".") {
					n, _ := strconv.Atoi(part)
					version = append(version, n)
				}
				if match[1] != "" {
					line = append(line, match[1])
				}
				continue
			}
		}
		if isDate(token) || token == "preview" || token == "latest" {
			continue
		}
		line = append(line, token)
	}
	return strings.Join(line, " "), version
}

func isDate(token string) bool {
	if len(token) < 4 {
		return false
	}
	for _, r := range token {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func compareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func hasID(models []router.Model, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func firstPositive(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}
