// Command server runs the real HTTP entry point: POST /chat runs
// classify -> check spend lock -> route -> generate -> record spend end
// to end against a real vendor API. main.go at the repo root stays a
// router-only demo; this is the first thing in the repo that actually
// serves requests.
package main

import (
	"log"
	"net/http"
	"os"

	"neochat/classifier"
	"neochat/limits"
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

	openRouterKey := os.Getenv("OPENROUTER_API_KEY")
	if openRouterKey == "" {
		log.Fatal("OPENROUTER_API_KEY is required (no key, no real vendor calls -- see provider.OpenRouterClient)")
	}
	openRouterClient := provider.NewOpenRouterClient(openRouterKey)

	// google/gemini-3.5-flash-lite is the default: it's the model
	// prompts/classifier_system_prompt.md was validated against (see
	// docs/unit-economics.md's assumptions table). Override with
	// CLASSIFIER_API_MODEL_ID for a different OpenRouter slug.
	classifierModelID := os.Getenv("CLASSIFIER_API_MODEL_ID")
	if classifierModelID == "" {
		classifierModelID = "google/gemini-3.5-flash-lite"
	}

	srv := &server.Server{
		Router:     router.NewRouter(catalog, weights),
		Classifier: classifier.New(openRouterClient, classifierModelID, systemPrompt),
		Store:      limits.NewInMemorySpendStore(),
		Plans:      plans,
		// Only "google" has a real client wired up right now. None of the
		// catalog's entries have a verified OpenRouter slug yet (see
		// README "Next steps"), so routing to any of them for generation
		// will still fail at generate time with a clear error until an
		// entry's api_model_id is confirmed against the real OpenRouter
		// catalog -- this map entry is ready for that, not tied to any
		// specific model.
		Generators: map[string]provider.Client{"google": openRouterClient},
	}

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, srv.Mux()))
}
