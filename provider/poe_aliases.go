package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
)

var poeHandlePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)

// ParsePoeBotAliases configures explicit, operator-owned public bot handles.
// It never chooses a replacement model or a third-party bot automatically.
func ParsePoeBotAliases(raw string) (map[string]string, error) {
	aliases := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return aliases, nil
	}
	var input map[string]string
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return nil, fmt.Errorf("POE_BOT_ALIASES must be a JSON object: %w", err)
	}
	for model, bot := range input {
		model = strings.TrimSpace(model)
		bot = strings.TrimSpace(bot)
		if !poeHandlePattern.MatchString(model) || !poeHandlePattern.MatchString(bot) {
			return nil, fmt.Errorf("POE_BOT_ALIASES contains an invalid model or bot handle")
		}
		aliases[strings.ToLower(model)] = bot
	}
	return aliases, nil
}
func (c *PoeClient) resolveBot(model string) string {
	model = poeModelName(model)
	if bot := c.BotAliases[strings.ToLower(model)]; bot != "" {
		return bot
	}
	return model
}

func (c *PoeClient) fallbackBot(model string, err error) string {
	var status *StatusError
	var denied *BotAccessDenied
	if !errors.As(err, &denied) && (!errors.As(err, &status) || status.StatusCode != 404) {
		return ""
	}
	bot := c.BotFallbacks[strings.ToLower(poeModelName(model))]
	if strings.EqualFold(bot, c.resolveBot(model)) {
		return ""
	}
	return bot
}

// Bind the requested model's parameter metadata before an alias/fallback
// changes its handle. Private prompt-bot names are absent from /v1/models.
// Each public entry point replaces this value, so helper calls cannot inherit
// the main model's controls and concurrent users never share request state.
type poeRequestModelInfoKey struct{}

func (c *PoeClient) requestModelContext(ctx context.Context, model string) context.Context {
	return context.WithValue(ctx, poeRequestModelInfoKey{}, c.info(poeModelName(model)))
}

func (c *PoeClient) requestModelInfo(ctx context.Context, model string) PoeModelInfo {
	if info, ok := ctx.Value(poeRequestModelInfoKey{}).(PoeModelInfo); ok {
		return info
	}
	return c.info(model)
}

func (c *PoeClient) Generate(ctx context.Context, model string, messages []Message) (GenerateResult, error) {
	ctx = c.requestModelContext(ctx, model)
	result, err := c.generatePoe(ctx, model, messages)
	if bot := c.fallbackBot(model, err); bot != "" {
		log.Printf("provider: Poe bot fallback model=%s bot=%s after pre-generation refusal", model, bot)
		return c.generatePoe(ctx, bot, messages)
	}
	return result, err
}
func (c *PoeClient) GenerateStream(ctx context.Context, model string, messages []Message) (<-chan StreamChunk, error) {
	ctx = c.requestModelContext(ctx, model)
	stream, err := c.streamPoe(ctx, model, messages)
	// Only initial HTTP refusal is eligible. Never replay an accepted stream.
	if bot := c.fallbackBot(model, err); bot != "" {
		log.Printf("provider: Poe bot fallback model=%s bot=%s after pre-generation refusal", model, bot)
		return c.streamPoe(ctx, bot, messages)
	}
	return stream, err
}
