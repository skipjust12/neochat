// Command server runs the real HTTP entry point: POST /chat runs
// classify + moderate -> check spend lock -> route -> generate -> record
// spend end to end against a real vendor API. main.go at the repo root
// stays a router-only demo; this is the first thing in the repo that
// actually serves requests.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"neochat/classifier"
	"neochat/conversation"
	"neochat/limits"
	"neochat/moderation"
	"neochat/provider"
	"neochat/router"
	"neochat/server"
)

func main() {
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

	srv := &server.Server{
		Router:     router.NewRouter(catalog, weights),
		Classifier: classifier.New(openRouterClient, classifierModelID, systemPrompt),
		Moderator:  moderation.New(openRouterClient, moderationModelID, moderationSystemPrompt),
		// InMemoryBlockLog is a throwaway stand-in, same as
		// limits.InMemorySpendStore below -- see moderation.BlockLog's doc
		// comment for the swap-in procedure once a real database exists.
		ModerationLog: moderation.NewInMemoryBlockLog(),
		// Same throwaway-stand-in story as ModerationLog/Store -- see
		// conversation.Store's doc comment for the Postgres swap-in
		// procedure once there's a real database to justify it.
		Conversations: conversation.NewInMemoryStore(),
		Store:         limits.NewInMemorySpendStore(),
		Plans:         plans,
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
