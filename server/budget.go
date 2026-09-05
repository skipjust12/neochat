package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"time"

	"neochat/limits"
	"neochat/provider"
	"neochat/router"
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
	}
	if maxOutput <= 0 || maxOutput > defaultOutputLimit {
		maxOutput = defaultOutputLimit
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
	refused := errors.As(callErr, &circuitErr) || (errors.As(callErr, &statusErr) && (statusErr.StatusCode == http.StatusTooManyRequests || statusErr.StatusCode == http.StatusBadRequest || statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusForbidden))
	if callErr != nil && !refused {
		log.Printf("server: retained reservation %s user_id=%s model=%s for uncertain vendor usage", reservation.ID, req.UserID, modelID)
		return result, callErr
	}
	actual := 0.0
	if !refused {
		if result.InputTokens < 0 || result.OutputTokens < 0 || (amount > 0 && (result.InputTokens == 0 || (result.Text != "" && result.OutputTokens == 0))) {
			return result, fmt.Errorf("provider returned missing or invalid usage; reservation retained")
		}
		actual = router.ComputeCostUSDRates(inputRate, outputRate, result.InputTokens, result.OutputTokens)
	}
	billCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.Store.Settle(billCtx, reservation, actual); err != nil {
		return result, fmt.Errorf("settle spend: %w", err)
	}
	return result, callErr
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
	switch req.RequestedMode {
	case "auto", "instant", "thinking", "max":
	case "manual":
		if _, ok := s.Router.Catalog.FindModel(req.ManualModelID); !ok {
			return fmt.Errorf("%w: unknown manual model", errInvalidRequest)
		}
	default:
		return fmt.Errorf("%w: unknown requested mode", errInvalidRequest)
	}
	if len(req.IdempotencyKey) > 128 || len(req.ConversationID) > 128 {
		return fmt.Errorf("%w: identifier too long", errInvalidRequest)
	}
	return nil
}

var errInvalidRequest = errors.New("invalid request")
