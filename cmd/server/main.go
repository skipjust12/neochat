// Command server runs the real HTTP entry point: POST /chat runs
// classify + moderate -> check spend lock -> route -> generate -> record
// spend end to end against a real vendor API. main.go at the repo root
// stays a router-only demo; this is the first thing in the repo that
// actually serves requests.
package main

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"neochat/auth"
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
	defer pgDB.Close()
	redisClient, err := db.ConnectRedis(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer redisClient.Close()

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
	// Override with OPENROUTER_MAX_TOKENS in the range 1..16000. The
	// reservation layer also enforces a per-model cap on every vendor call.
	openRouterClient.MaxTokens = getenvIntDefault("OPENROUTER_MAX_TOKENS", 16000)
	if openRouterClient.MaxTokens <= 0 || openRouterClient.MaxTokens > 16000 {
		log.Fatal("OPENROUTER_MAX_TOKENS must be between 1 and 16000")
	}

	// Every vendor call -- generation, classify, moderate, summarize --
	// goes through the retry wrapper: OpenRouter really does hand out 429s
	// (README's Status section records hitting them during live testing),
	// and before this a single one failed the user's request outright. The
	// policy is deliberately narrow about what it resends, since a chat
	// completion is not idempotent -- see provider.StatusError.Retryable.
	retryingClient := provider.NewRetryClient(openRouterClient)

	// Generation additionally goes through a circuit breaker so a model
	// that starts failing en masse (README pre-launch checklist) gets
	// excluded for everyone for a cooldown, instead of every request
	// separately discovering the same failure via its own timeout. State
	// is tracked per apiModelID inside the breaker, so one instance shared
	// across every catalog provider (Generators below) still isolates each
	// model's health independently.
	//
	// The breaker wraps the retrier, not the other way round: a failure
	// should count against a model once the request has genuinely given
	// up, not once per intermediate attempt. Inverted, one burst of rate
	// limiting would trip the breaker and pull the model for everyone --
	// converting a recoverable blip into an outage.
	generationClient := provider.NewCircuitBreakerClient(retryingClient, 5, 30*time.Second)

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
		if d <= 0 {
			log.Fatal("IDEMPOTENCY_TTL must be positive")
		}
		idempotencyTTL = d
	}

	classifierWithCost := classifier.New(retryingClient, classifierModelID, systemPrompt)
	classifierWithCost.CostInputPerMTok = classifierCostInputPerMTok
	classifierWithCost.CostOutputPerMTok = classifierCostOutputPerMTok

	moderatorWithCost := moderation.New(retryingClient, moderationModelID, moderationSystemPrompt)
	moderatorWithCost.CostInputPerMTok = moderationCostInputPerMTok
	moderatorWithCost.CostOutputPerMTok = moderationCostOutputPerMTok

	// Two request throttles on POST /chat and /chat/stream, over different
	// keys -- see server.Server.IPRateLimiter/UserRateLimiter and audit.md
	// finding #2. The per-IP one catches a caller with no valid credential
	// at all (the per-user one never sees those, since it runs after
	// authentication); the per-user one follows a single key across many
	// source addresses (which no per-IP counter can see). Distinct prefixes
	// keep their Redis keyspaces apart.
	//
	// Both default to a deliberately generous 60/min: there are no measured
	// traffic patterns to tune against yet, and a throttle that
	// false-positives on real use is worse than a loose one at this stage.
	// The per-user limit is the one worth tightening first once usage data
	// exists -- a human sends single-digit messages per minute, so its
	// real ceiling is far below 60, whereas the per-IP limit has to keep
	// headroom for however many users share one NAT.
	rateLimitPerMinute := getenvIntDefault("RATE_LIMIT_PER_MINUTE", 60)
	ipRateLimiter := ratelimit.NewRedisLimiter(redisClient, "chat_ip", rateLimitPerMinute, time.Minute)

	userRateLimitPerMinute := getenvIntDefault("USER_RATE_LIMIT_PER_MINUTE", 60)
	userRateLimiter := ratelimit.NewRedisLimiter(redisClient, "chat_user", userRateLimitPerMinute, time.Minute)

	trustedProxies := []netip.Prefix{}
	for _, raw := range strings.Split(os.Getenv("TRUSTED_PROXY_CIDRS"), ",") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			log.Fatal(err)
		}
		trustedProxies = append(trustedProxies, prefix)
	}
	summaryClient := summarizer.New(retryingClient, summarizerModelID, summarizerSystemPrompt)
	summaryClient.CostInputPerMTok = getenvFloatDefault("SUMMARIZER_COST_INPUT_PER_MTOK", classifierCostInputPerMTok)
	summaryClient.CostOutputPerMTok = getenvFloatDefault("SUMMARIZER_COST_OUTPUT_PER_MTOK", classifierCostOutputPerMTok)
	srv := &server.Server{
		TrustedProxies: trustedProxies,
		RequireHTTPS:   os.Getenv("REQUIRE_HTTPS") == "true",
		Router:         router.NewRouter(catalog, weights),
		Classifier:     classifierWithCost,
		Moderator:      moderatorWithCost,
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
		Summarizer:           summaryClient,
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
		IPRateLimiter:   ipRateLimiter,
		UserRateLimiter: userRateLimiter,
		// Postgres-backed -- see auth.Store's doc comment and audit.md
		// finding #1. Keys are minted out of band via `go run
		// ./cmd/issuekey` (see docs/running-locally.md); there is no HTTP
		// signup endpoint yet.
		Auth: auth.NewPostgresStore(pgDB),
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

	// Run the listener in a goroutine so main can watch for either it
	// failing outright or a shutdown signal arriving, whichever comes
	// first.
	serveErrs := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", addr)
		serveErrs <- srvHTTP.ListenAndServe()
	}()

	// SIGTERM is what `docker stop`/most orchestrators send before
	// escalating to SIGKILL after their grace period (docker-compose.yml
	// sets stop_grace_period: 30s on the server service to match
	// shutdownGracePeriod below); SIGINT covers a local Ctrl-C. Without
	// this, the previous log.Fatal(srvHTTP.ListenAndServe()) meant any
	// deploy/restart killed in-flight requests outright -- including a
	// /chat/stream generation that could legitimately be running for
	// minutes (see srvHTTP's WriteTimeout comment above) -- instead of
	// letting them finish.
	stopCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serveErrs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	case <-stopCtx.Done():
		// Restore default signal behavior so a second Ctrl-C/SIGTERM
		// force-kills immediately instead of the process ignoring it while
		// stuck waiting out shutdownGracePeriod.
		stop()
		log.Print("shutdown signal received, draining in-flight requests...")

		const shutdownGracePeriod = 25 * time.Second
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()
		if err := srvHTTP.Shutdown(shutdownCtx); err != nil {
			log.Printf("server: graceful shutdown did not finish cleanly: %v", err)
		}
	}
	log.Print("server stopped")
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
	if f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		log.Fatalf("%s must be finite and positive", key)
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
