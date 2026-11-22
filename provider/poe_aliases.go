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
	if !errors.As(err, &status) || status.StatusCode != 404 {
		return ""
	}
	bot := c.BotFallbacks[strings.ToLower(poeModelName(model))]
	if strings.EqualFold(bot, c.resolveBot(model)) {
		return ""
	}
	return bot
}
func (c *PoeClient) Generate(ctx context.Context, model string, messages []Message) (GenerateResult, error) {
	result, err := c.generatePoe(ctx, model, messages)
	if bot := c.fallbackBot(model, err); bot != "" {
		log.Printf("provider: Poe bot fallback model=%s bot=%s after HTTP 404", model, bot)
		return c.generatePoe(ctx, bot, messages)
	}
	return result, err
}
func (c *PoeClient) GenerateStream(ctx context.Context, model string, messages []Message) (<-chan StreamChunk, error) {
	stream, err := c.streamPoe(ctx, model, messages)
	// Only initial HTTP refusal is eligible. Never replay an accepted stream.
	if bot := c.fallbackBot(model, err); bot != "" {
		log.Printf("provider: Poe bot fallback model=%s bot=%s after HTTP 404", model, bot)
		return c.streamPoe(ctx, bot, messages)
	}
	return stream, err
}
