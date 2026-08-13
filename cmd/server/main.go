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
	"neochat/ratelimit"
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

	// Hard ceiling on output tokens for every generation call, regardless
	// of which catalog model gets selected -- see
	// provider.OpenRouterClient.MaxTokens's doc comment and audit.md
	// finding #4. 16000 is comfortably above any real chat reply while
	// staying well under the priciest catalog models' own
	// max_output_tokens (128000+, see configs/models.json), which is what
	// every call was implicitly allowed to run up to before this existed.
	// Override with OPENROUTER_MAX_TOKENS; 0 disables the cap entirely
	// (falls back to each model's own default).
	openRouterClient.MaxTokens = getenvIntDefault("OPENROUTER_MAX_TOKENS", 16000)

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

	// Per-Mtok rates for pricing classifier/moderation calls into cost_log
	// (README pre-launch checklist item 7's "still not logged" gap,
	// closed 2026-08-12) -- classifier.Classifier/moderation.Moderator
	// have no catalog entry to price against the way a generation model
	// does (moderation's default, gpt-oss-120b, was deliberately removed
	// from configs/models.json entirely), so these are their own
	// standalone rates. Defaults are docs/unit-economics.md's assumed
	// figures for the *_API_MODEL_ID defaults just above
	// (gemini-3.5-flash-lite $0.3/$2.5, gpt-oss-120b $0.03/$0.17); if you
	// override either *_API_MODEL_ID, override its cost rates too, or
	// logged cost will silently keep pricing the old model.
	classifierCostInputPerMTok := getenvFloatDefault("CLASSIFIER_COST_INPUT_PER_MTOK", 0.3)
	classifierCostOutputPerMTok := getenvFloatDefault("CLASSIFIER_COST_OUTPUT_PER_MTOK", 2.5)
	moderationCostInputPerMTok := getenvFloatDefault("MODERATION_COST_INPUT_PER_MTOK", 0.03)
	moderationCostOutputPerMTok := getenvFloatDefault("MODERATION_COST_OUTPUT_PER_MTOK", 0.17)

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

	classifierWithCost := classifier.New(openRouterClient, classifierModelID, systemPrompt)
	classifierWithCost.CostInputPerMTok = classifierCostInputPerMTok
	classifierWithCost.CostOutputPerMTok = classifierCostOutputPerMTok

	moderatorWithCost := moderation.New(openRouterClient, moderationModelID, moderationSystemPrompt)
	moderatorWithCost.CostInputPerMTok = moderationCostInputPerMTok
	moderatorWithCost.CostOutputPerMTok = moderationCostOutputPerMTok

	// Per-IP request throttle on POST /chat and /chat/stream -- see
	// server.Server.IPRateLimiter's doc comment and audit.md finding #2.
	// Deliberately IP-keyed rather than user_id-keyed: user_id is
	// client-supplied and unauthenticated today (see chatRequest.UserID's
	// doc comment), so this is the one layer that can't be sidestepped by
	// just claiming a different user_id on the next request.
	// RATE_LIMIT_PER_MINUTE overrides the per-IP request count; 60/min is a
	// generous placeholder for a service with no real traffic patterns
	// measured yet -- tighten once there's usage data to tune against.
	rateLimitPerMinute := getenvIntDefault("RATE_LIMIT_PER_MINUTE", 60)
	ipRateLimiter := ratelimit.NewRedisLimiter(redisClient, "chat_ip", rateLimitPerMinute, time.Minute)

	srv := &server.Server{
		Router:     router.NewRouter(catalog, weights),
		Classifier: classifierWithCost,
		Moderator:  moderatorWithCost,
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
		IPRateLimiter: ipRateLimiter,
	}

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// Explicit http.Server instead of http.ListenAndServe's zero-value one
	// -- the zero value leaves ReadTimeout/ReadHeaderTimeout/IdleTimeout
	// unset (no limit), which lets a client that opens a connection and
	// then sends data arbitrarily slowly (or never) hold it open forever,
	// exhausting server resources one slow connection at a time (a
	// Slowloris-style DoS -- see audit.md finding #3). WriteTimeout is
	// deliberately left unset: /chat/stream can legitimately take minutes
	// on a long "max" mode generation, and a blanket WriteTimeout would cut
	// those responses off mid-stream. maxRequestBodyBytes (server package)
	// already bounds how much a slow client can make the server buffer
	// regardless of how long ReadTimeout gives it to send that body.
	srvHTTP := &http.Server{
		Addr:              addr,
		Handler:           srv.Mux(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("listening on %s", addr)
	log.Fatal(srvHTTP.ListenAndServe())
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

// getenvFloatDefault returns the float64 value of the named env var, or
// def if it's unset. A set-but-unparseable value fails startup loudly
// (log.Fatal) rather than silently falling back to def, the same
// treatment every other malformed override in this file gets (e.g.
// SUMMARY_TAIL_MESSAGES above).
func getenvFloatDefault(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Fatalf("%s: %v", key, err)
	}
	return f
}

// getenvIntDefault is getenvFloatDefault's int counterpart -- same
// "unset falls back to def, set-but-unparseable fails startup loudly"
// contract.
func getenvIntDefault(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("%s: %v", key, err)
	}
	return n
}
