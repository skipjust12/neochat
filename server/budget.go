package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"neochat/attachment"
	"neochat/limits"
	"neochat/provider"
	"neochat/router"
	"neochat/tokenizer"
)

const defaultOutputLimit = 16000

func generationPool(result router.RouteResult, model router.Model) limits.Pool {
	if result.SelectedMode == "instant" {
		return limits.PoolInstant
	}
	if result.SelectedMode == "manual" {
		for _, mode := range model.Modes {
			if mode == "instant" {
				return limits.PoolInstant
			}
		}
	}
	return limits.PoolThinkingMax
}

// billedCall reserves a byte-based upper estimate before contacting a vendor.
// Text tokenizers use at most one token per byte; framing gets extra headroom.
// Prices must match the deployed model. Missing usage/errors retain the reserve
// for operator reconciliation rather than granting another unaccounted call.
func (s *Server) billedCall(ctx context.Context, req chatRequest, plan limits.PlanLimits, pool limits.Pool, inputRate, outputRate float64, maxOutput int, gen provider.Client, modelID string, messages []provider.Message, call generateCaller) (provider.GenerateResult, error) {
	if err := ctx.Err(); err != nil {
		return provider.GenerateResult{}, err
	}
	for _, rate := range []float64{inputRate, outputRate} {
		if rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return provider.GenerateResult{}, fmt.Errorf("invalid model price")
		}
	}
	inputBound := 256
	for _, m := range messages {
		inputBound += len(m.Role) + len(m.Content) + 64
		// Images and documents have no byte-per-token relation; their
		// estimates are doubled to stay an upper bound.
		for _, p := range m.Parts {
			inputBound += 2 * tokenizer.EstimateAttachment(p)
		}
	}
	if maxOutput <= 0 || maxOutput > defaultOutputLimit {
		maxOutput = defaultOutputLimit
	}
	// The web plugin puts its search results into the prompt. (Tool-loop
	// results are ordinary messages, counted above.)
	if p, ok := provider.WebPluginFromContext(ctx); ok {
		results := p.MaxResults
		if results <= 0 {
			results = 5
		}
		inputBound += results * 600
	}
	amount := router.ComputeCostUSDRates(inputRate, outputRate, inputBound, maxOutput)
	cap := plan.ThinkingMaxCapUSD
	if pool == limits.PoolInstant {
		cap = plan.InstantExtraCapUSD
	}
	reservation, err := s.Store.Reserve(ctx, req.UserID, pool, amount, cap)
	if err != nil {
		return provider.GenerateResult{}, err
	}
	result, callErr := call(provider.WithOutputLimit(ctx, maxOutput), gen, modelID, messages)
	// Only a known refusal is safe to refund without usage.
	var circuitErr *provider.CircuitOpenError
	var statusErr *provider.StatusError
	refused := errors.As(callErr, &circuitErr) || (errors.As(callErr, &statusErr) && (statusErr.StatusCode == http.StatusTooManyRequests || statusErr.StatusCode == http.StatusBadRequest || statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusPaymentRequired || statusErr.StatusCode == http.StatusForbidden || statusErr.StatusCode == http.StatusNotFound)) || errors.Is(callErr, provider.ErrMissingAPIKey)
	// The user pressed Stop (the request context was canceled): the vendor
	// stops generating when the connection drops, so settle on an estimate
	// of what was actually produced instead of holding the full upper
	// reservation -- otherwise every Stop on an expensive model would eat a
	// worst-case chunk of the plan cap. Input is charged at its byte-based
	// upper bound (the prompt was already processed, so a stop is never
	// free); hidden reasoning tokens can't be seen here, so the visible
	// output is doubled.
	stopped := callErr != nil && !refused && errors.Is(ctx.Err(), context.Canceled)
	if callErr != nil && !refused && !stopped {
		log.Printf("server: retained reservation %s user_id=%s model=%s for uncertain vendor usage", reservation.ID, req.UserID, modelID)
		return result, callErr
	}
	actual := 0.0
	if stopped {
		actual = math.Min(amount, router.ComputeCostUSDRates(inputRate, outputRate, inputBound, 2*tokenizer.EstimateText(result.Text)))
	} else if !refused {
		if result.InputTokens < 0 || result.OutputTokens < 0 || (amount > 0 && (result.InputTokens == 0 || (result.Text != "" && result.OutputTokens == 0))) {
			return result, fmt.Errorf("provider returned missing or invalid usage; reservation retained")
		}
		actual = callCostUSD(inputRate, outputRate, result)
	}
	billCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.Store.Settle(billCtx, reservation, actual); err != nil {
		return result, fmt.Errorf("settle spend: %w", err)
	}
	return result, callErr
}

// callCostUSD is what a vendor call cost: what Polza charged for it when
// it says -- prompt cache discounts and server tools included -- else the
// list price of its tokens, with cache writes at Claude's 1.25x (the most
// any vendor charges for them; reads are counted in full) plus the web
// plugin fee.
func callCostUSD(inputRate, outputRate float64, result provider.GenerateResult) float64 {
	if result.CostRUB > 0 {
		return result.CostRUB / rubPerUSD
	}
	cost := router.ComputeCostUSDRates(inputRate, outputRate, result.InputTokens, result.OutputTokens)
	cost += 0.25 * router.ComputeCostUSDRates(inputRate, 0, result.CacheWriteTokens, 0)
	return cost + float64(result.WebSearches)*webPluginFeeUSD
}

// auxiliaryClient is local to a request; no shared Server fields are mutated.
type auxiliaryClient struct {
	server     *Server
	request    chatRequest
	plan       limits.PlanLimits
	client     provider.Client
	inputRate  float64
	outputRate float64
}

func (c auxiliaryClient) Generate(ctx context.Context, modelID string, messages []provider.Message) (provider.GenerateResult, error) {
	return c.server.billedCall(ctx, c.request, c.plan, limits.PoolInstant, c.inputRate, c.outputRate, defaultOutputLimit, c.client, modelID, messages, directGenerate)
}

func (s *Server) validateRequest(req chatRequest) error {
	if s.RouterDisabled {
		if req.RequestedMode != "manual" {
			return errRouterDisabled
		}
		// An image model runs on the image key alone (image.go).
		model, found := s.router().Catalog.FindModel(req.ManualModelID)
		switch {
		case found && model.Kind == router.KindImage:
			if req.imageKey == "" {
				return errMissingImageKey
			}
		case req.providerKey == "":
			return errMissingProviderKey
		}
	}
	for _, key := range []string{req.providerKey, req.imageKey} {
		if len(key) > 512 || strings.ContainsAny(key, " \t\r\n") {
			return fmt.Errorf("%w: malformed provider API key", errInvalidRequest)
		}
	}
	if len(req.Attachments) > attachment.MaxPerMessage {
		return userErrorf("You can attach up to %d files to one message.", attachment.MaxPerMessage)
	}
	if len(req.Attachments) > 0 && req.Incognito {
		return userErrorf("Files can't be attached in incognito chats yet.")
	}
	seen := map[string]bool{}
	for _, id := range req.Attachments {
		if id == "" || len(id) > 64 || seen[id] {
			return fmt.Errorf("%w: invalid attachment id", errInvalidRequest)
		}
		seen[id] = true
	}
	if !validWebSearchMode(req.WebSearch) {
		return fmt.Errorf("%w: web_search must be auto, on or off", errInvalidRequest)
	}
	if req.ReasoningEffort != "" && !reasoningEfforts[req.ReasoningEffort] {
		return fmt.Errorf("%w: unknown reasoning effort", errInvalidRequest)
	}
	if len([]rune(req.Instructions)) > maxInstructionsRunes {
		return fmt.Errorf("%w: custom instructions exceed %d characters", errInvalidRequest, maxInstructionsRunes)
	}
	switch req.RequestedMode {
	case "auto", "instant", "thinking", "max":
	case "manual":
		if _, ok := s.router().Catalog.FindModel(req.ManualModelID); !ok {
			return fmt.Errorf("%w: unknown manual model", errInvalidRequest)
		}
	default:
		return fmt.Errorf("%w: unknown requested mode", errInvalidRequest)
	}
	if len(req.IdempotencyKey) > 128 || len(req.ConversationID) > 128 || len(req.ProjectID) > 128 {
		return fmt.Errorf("%w: identifier too long", errInvalidRequest)
	}
	if req.Incognito && req.ConversationID != "" {
		return fmt.Errorf("%w: incognito requests cannot use a saved conversation", errInvalidRequest)
	}
	if req.Incognito && req.ProjectID != "" {
		return fmt.Errorf("%w: incognito requests cannot use a project", errInvalidRequest)
	}
	if !req.Incognito && len(req.IncognitoHistory) > 0 {
		return fmt.Errorf("%w: incognito history requires incognito mode", errInvalidRequest)
	}
	if len(req.IncognitoHistory) > 50 {
		return fmt.Errorf("%w: incognito history is too long", errInvalidRequest)
	}
	for _, message := range req.IncognitoHistory {
		if (message.Role != "user" && message.Role != "assistant") || strings.TrimSpace(message.Content) == "" {
			return fmt.Errorf("%w: invalid incognito history", errInvalidRequest)
		}
	}
	return nil
}

var errInvalidRequest = errors.New("invalid request")

// errRouterDisabled rejects every non-Manual request while the auto-router
// is switched off (Server.RouterDisabled). Its text is shown to the user.
var errRouterDisabled = errors.New("Router disabled: switch to Manual mode and pick a model")

// errMissingProviderKey means the user hasn't added their Polza AI key in
// Settings yet. Its text is shown to the user.
var errMissingProviderKey = errors.New("Add your Polza AI API key in Settings → Account to start chatting")

// reasoningFor maps the UI's effort slider onto what model accepts.
// Models with no reasoning control get nothing (the setting would be
// silently dropped anyway). Adaptive Claude models only know
// low/medium/high/max, so xhigh rounds up to max -- the user asked for
// more than high.
func reasoningFor(model router.Model, effort string) (provider.Reasoning, bool) {
	if effort == "" {
		return provider.Reasoning{}, false
	}
	switch model.Reasoning {
	case "effort":
		return provider.Reasoning{Effort: effort}, true
	case "adaptive":
		if effort == "xhigh" {
			effort = "max"
		}
		return provider.Reasoning{Effort: effort, Adaptive: true}, true
	}
	return provider.Reasoning{}, false
}
