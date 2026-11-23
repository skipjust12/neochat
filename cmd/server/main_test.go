package main

import (
	"neochat/router"
	"testing"
)

func TestFreeModelPricingCanBeZero(t *testing.T) {
	for _, key := range []string{
		"SUMMARIZER_COST_INPUT_PER_MTOK", "SUMMARIZER_COST_OUTPUT_PER_MTOK",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "0")
			if got := getenvFloatDefault(key, 0.3); got != 0 {
				t.Fatalf("free model price = %v, want 0", got)
			}
		})
	}
}

func TestTerminalIsOnByDefaultForEveryone(t *testing.T) {
	t.Setenv("TERMINAL_ENABLED", "")
	t.Setenv("TERMINAL_USERS", "")
	r := terminalRunner()
	if r == nil || !r.Allowed("anyone") {
		t.Fatalf("runner = %+v", r)
	}
	t.Setenv("TERMINAL_USERS", "me")
	if r := terminalRunner(); r.Allowed("anyone") || !r.Allowed("me") {
		t.Fatal("TERMINAL_USERS not applied")
	}
	t.Setenv("TERMINAL_ENABLED", "false")
	if terminalRunner() != nil {
		t.Fatal("TERMINAL_ENABLED=false should turn it off")
	}
}

func TestSummaryTriggerIgnoresMediaAndUncalibratedBots(t *testing.T) {
	catalog := router.Catalog{Models: []router.Model{{ID: "text", ContextWindow: 200000}, {ID: "video", Kind: router.KindVideo, ContextWindow: 480}, {ID: "new-bot", ManualOnly: true, ContextWindow: 1024}}}
	if got := smallestContextWindow(catalog); got != 200000 {
		t.Fatalf("summary context=%d", got)
	}
}
