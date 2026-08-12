// Command server runs the real HTTP entry point: POST /chat runs
// classify + moderate -> check spend lock -> route -> generate -> record
// spend end to end against a real vendor API. main.go at the repo root
// stays a router-only demo; this is the first thing in the repo that
// actually serves requests.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"neochat/classifier"
	"neochat/conversation"
	"neochat/costlog"
	"neochat/db"
	"neochat/idempotency"
	"neochat/internal/envfile"
	"neochat/limits"
	"neochat/moderation"
	"neochat/provider"
	"neochat/router"
	"neochat/server"
	"neochat/summarizer"
)

func main() {
	// .env is optional (real deployments set env vars directly) and never
	// overrides a variable already set in the real environment -- see
	// internal/envfile's doc comment.
	if err := envfile.Load(".env"); err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	pgDB, err := db.Connect(ctx)
	if err != nil {
		log.Fatal(err)
	}
	redisClient, err := db.ConnectRedis(ctx)
	if err != nil {
		log.Fatal(err)
	}

	catalog, err := router.LoadCatalog("configs/models.json")
	if err != nil {
		log.Fatal(err)
	}
	weights, err := router.LoadWeights("configs/weights.json")
	if err != nil {
		log.Fatal(err)
	}
	plans, err := limits.LoadPlanLimits("configs/plans.json")
	if err != nil {
		log.Fatal(err)
	}
	systemPrompt, err := classifier.LoadSystemPrompt("prompts/classifier_system_prompt.md")
	if err != nil {
		log.Fatal(err)
	}
	moderationSystemPrompt, err := moderation.LoadSystemPrompt("prompts/moderation_system_prompt.md")
	if err != nil {
		log.Fatal(err)
	}
	// Chat system prompts are per-persona (server.SystemPromptNames --
	// "Default", "Expert", "Friendly", "Cynical", "Direct"), loaded from
	// prompts/<Name>.md. Unlike the classifier/moderation prompts above, a
	// persona whose file is missing/empty isn't fatal: LoadSystemPrompts
	// just skips it, and selecting that persona sends no system message
	// until its real prompt text is written.
	chatSystemPrompts := server.LoadSystemPrompts("prompts")

	openRouterKey := os.Getenv("OPENROUTER_API_KEY")
	if openRouterKey == "" {
		log.Fatal("OPENROUTER_API_KEY is required (no key, no real vendor calls -- see provider.OpenRouterClient)")
	}
	openRouterClient := provider.NewOpenRouterClient(openRouterKey)

	// Generation goes through a circuit breaker so a model that starts
	// failing en masse (README pre-launch checklist) gets excluded for
	// everyone for a cooldown, instead of every request separately
	// discovering the same failure via its own timeout. State is tracked
	// per apiModelID inside the breaker, so one instance shared across
	// every catalog provider (Generators below) still isolates each
	// model's health independently. Not wired into classifier/moderation
	// calls yet -- same wrapper would apply trivially, just lower
	// priority than the generation path this protects first.
	generationClient := provider.NewCircuitBreakerClient(openRouterClient, 5, 30*time.Second)

	// google/gemini-3.5-flash-lite is the default: it's the model
	// prompts/classifier_system_prompt.md was validated against (see
	// docs/unit-economics.md's assumptions table). Override with
	// CLASSIFIER_API_MODEL_ID for a different OpenRouter slug.
	classifierModelID := os.Getenv("CLASSIFIER_API_MODEL_ID")
	if classifierModelID == "" {
		classifierModelID = "google/gemini-3.5-flash-lite"
	}

	// openai/gpt-oss-120b is the default: it's the model
	// docs/unit-economics.md assumed for both moderation layers. Override
	// with MODERATION_API_MODEL_ID for a different OpenRouter slug.
	moderationModelID := os.Getenv("MODERATION_API_MODEL_ID")
	if moderationModelID == "" {
		moderationModelID = "openai/gpt-oss-120b"
	}

	summarizerSystemPrompt, err := summarizer.LoadSystemPrompt("prompts/summarizer_system_prompt.md")
	if err != nil {
		log.Fatal(err)
	}

	// Same cheap model as the classifier (SUMMARIZER_API_MODEL_ID
	// overrides independently) -- see summarizer package doc comment.
	summarizerModelID := os.Getenv("SUMMARIZER_API_MODEL_ID")
	if summarizerModelID == "" {
		summarizerModelID = classifierModelID
	}

	// SummaryTailMessages: how many of the most recent messages are
	// always sent verbatim -- see server.Server.SummaryTailMessages.
	summaryTailMessages := 10
	if v := os.Getenv("SUMMARY_TAIL_MESSAGES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Fatalf("SUMMARY_TAIL_MESSAGES: %v", err)
		}
		summaryTailMessages = n
	}

	// SummaryTriggerTokens defaults to 60% of the smallest catalog
	// model's ContextWindow, so summarization kicks in before
	// router.applyHardFilters would reject every model for a growing
	// conversation -- see server.Server.SummaryTriggerTokens.
	// SUMMARY_TRIGGER_TOKENS overrides it directly.
	summaryTriggerTokens := smallestContextWindow(catalog) * 60 / 100
	if v := os.Getenv("SUMMARY_TRIGGER_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Fatalf("SUMMARY_TRIGGER_TOKENS: %v", err)
		}
		summaryTriggerTokens = n
	}

	// How long a completed idempotency response stays replayable in Redis
	// -- see idempotency.Store's doc comment on why a real deployment
	// needs a TTL here (InMemoryStore kept entries forever).
	idempotencyTTL := 24 * time.Hour
	if v := os.Getenv("IDEMPOTENCY_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("IDEMPOTENCY_TTL: %v", err)
		}
		idempotencyTTL = d
	}

	srv := &server.Server{
		Router:     router.NewRouter(catalog, weights),
		Classifier: classifier.New(openRouterClient, classifierModelID, systemPrompt),
		Moderator:  moderation.New(openRouterClient, moderationModelID, moderationSystemPrompt),
		// Postgres-backed, replacing InMemoryBlockLog -- see
		// moderation.BlockLog's doc comment for the swap-in procedure this
		// follows and README's "Current task" section.
		ModerationLog: moderation.NewPostgresBlockLog(pgDB),
		// Postgres-backed, replacing InMemoryStore -- see
		// conversation.Store's doc comment.
		Conversations: conversation.NewPostgresStore(pgDB),
		// Redis-backed, replacing InMemorySpendStore -- see
		// limits.SpendStore's doc comment.
		Store: limits.NewRedisSpendStore(redisClient),
		// Redis-backed, replacing InMemoryStore -- see idempotency.Store's
		// doc comment. IDEMPOTENCY_TTL overrides how long a completed
		// response stays replayable.
		Idempotency: idempotency.NewRedisStore(redisClient, idempotencyTTL),
		// Postgres-backed, replacing InMemoryStore -- see costlog.Store's
		// doc comment.
		CostLog:              costlog.NewPostgresStore(pgDB),
		Plans:                plans,
		SystemPrompts:        chatSystemPrompts,
		Summarizer:           summarizer.New(openRouterClient, summarizerModelID, summarizerSystemPrompt),
		SummaryTriggerTokens: summaryTriggerTokens,
		SummaryTailMessages:  summaryTailMessages,
		// Every catalog provider tag routes through the same
		// OpenRouterClient -- OpenRouter serves all of them, so there's no
		// need for a second provider.Client implementation (see README
		// "OpenRouter as a single point of failure" and "Next steps").
		// Each catalog entry's api_model_id (configs/models.json) now
		// carries a real OpenRouter slug for its provider.
		Generators: map[string]provider.Client{
			"anthropic": generationClient,
			"openai":    generationClient,
			"google":    generationClient,
			"moonshot":  generationClient,
			"deepseek":  generationClient,
		},
	}

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, srv.Mux()))
}

// smallestContextWindow returns the smallest ContextWindow across every
// model in catalog, used to derive a sensible default for
// SUMMARY_TRIGGER_TOKENS. Panics on an empty catalog -- router.LoadCatalog
// already fails startup before this point if configs/models.json has no
// models, so this is an invariant, not a runtime condition to handle.
func smallestContextWindow(catalog router.Catalog) int {
	if len(catalog.Models) == 0 {
		log.Fatal("smallestContextWindow: catalog has no models")
	}
	min := catalog.Models[0].ContextWindow
	for _, m := range catalog.Models[1:] {
		if m.ContextWindow < min {
			min = m.ContextWindow
		}
	}
	return min
}
