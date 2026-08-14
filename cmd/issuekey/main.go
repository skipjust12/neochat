// Command issuekey mints a new neochat API key for one user and prints it
// once. This is the entire "how does a user get a key" story for now --
// there is no signup flow, no login, no self-serve anything (see auth
// package doc comment). An operator runs this by hand per early user/
// tester and sends them the printed token out of band.
//
// Replacing this with a real signup endpoint later means calling the
// same auth.Store.IssueKey from an HTTP handler instead of from this
// CLI's main -- the storage/lookup side (auth.PostgresStore, the
// api_keys table, server.go's authenticated middleware) doesn't change;
// only how a caller triggers issuance does.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"neochat/auth"
	"neochat/db"
	"neochat/internal/envfile"
	"neochat/limits"
)

func main() {
	userID := flag.String("user_id", "", "user_id to mint a key for (required)")
	planID := flag.String("plan_id", "", "plan_id to attach to the key -- must exist in configs/plans.json (required)")
	plansPath := flag.String("plans", "configs/plans.json", "path to plans.json, used only to validate -plan_id")
	flag.Parse()

	if *userID == "" || *planID == "" {
		flag.Usage()
		log.Fatal("issuekey: -user_id and -plan_id are both required")
	}

	// Validated against the same plans.json cmd/server loads, so a typo'd
	// -plan_id fails loudly here instead of silently minting a key whose
	// every /chat request later 500s on decodeChatRequest's plan lookup.
	plans, err := limits.LoadPlanLimits(*plansPath)
	if err != nil {
		log.Fatal(err)
	}
	if _, ok := plans[*planID]; !ok {
		log.Fatalf("issuekey: unknown plan_id %q (see %s)", *planID, *plansPath)
	}

	// .env is optional, same convention as cmd/server -- see
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

	token, err := auth.NewPostgresStore(pgDB).IssueKey(ctx, *userID, *planID)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("API key for user_id=%s plan_id=%s -- shown once, store it now, it cannot be recovered later:\n%s\n", *userID, *planID, token)
}
