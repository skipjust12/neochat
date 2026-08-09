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

	openAIKey := os.Getenv("OPENAI_API_KEY")
	if openAIKey == "" {
		log.Fatal("OPENAI_API_KEY is required (no key, no real vendor calls -- see provider.OpenAIClient)")
	}
	openAIClient := provider.NewOpenAIClient(openAIKey)

	classifierModelID := os.Getenv("CLASSIFIER_API_MODEL_ID")
	if classifierModelID == "" {
		log.Fatal("CLASSIFIER_API_MODEL_ID is required (the exact vendor-side model string to classify with)")
	}

	srv := &server.Server{
		Router:     router.NewRouter(catalog, weights),
		Classifier: classifier.New(openAIClient, classifierModelID, systemPrompt),
		Store:      limits.NewInMemorySpendStore(),
		Plans:      plans,
		// Only "openai" has a real client wired up right now -- routing
		// to a model from any other catalog provider (anthropic, google,
		// ...) will fail at generate time with a clear error until a
		// provider.Client exists for it. See README "Provider abstraction
		// layer".
		Generators: map[string]provider.Client{"openai": openAIClient},
	}

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, srv.Mux()))
}
