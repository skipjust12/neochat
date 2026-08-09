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

	classifierModelID := os.Getenv("CLASSIFIER_API_MODEL_ID")
	if classifierModelID == "" {
		log.Fatal("CLASSIFIER_API_MODEL_ID is required (the exact OpenRouter model slug, e.g. \"vendor/model-name\", to classify with)")
	}

	srv := &server.Server{
		Router:     router.NewRouter(catalog, weights),
		Classifier: classifier.New(openRouterClient, classifierModelID, systemPrompt),
		Store:      limits.NewInMemorySpendStore(),
		Plans:      plans,
		// Only "google" has a real client wired up right now -- it's the
		// provider tag on configs/models.json's temporary
		// "gemma-4-31b-it:free" test entry (see docs/running-locally.md).
		// Routing to a model tagged with any other catalog provider will
		// fail at generate time with a clear error until either that
		// entry is given a verified OpenRouter slug (api_model_id) and
		// added here, or this map is built from every catalog provider at
		// once -- OpenRouter can serve all of them through this same
		// client, the missing piece is confirming each one's real slug,
		// not writing another Client implementation.
		Generators: map[string]provider.Client{"google": openRouterClient},
	}

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, srv.Mux()))
}
