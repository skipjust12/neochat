package modelcatalog

import "testing"

func TestPoeNamesKeepAPIAndDisplaySeparate(t *testing.T) {
	for input, want := range map[string]string{"Gemini-4-Hydrogen": "Gemini 4 Hydrogen", "GPT-6.1-Sol-Pro": "GPT-6.1 Sol Pro", "Claude-Opus-5.5": "Claude Opus 5.5", "Nano-Banana-2": "Nano Banana 2", "Gemma-4-26B-A4B": "Gemma 4 26B A4B"} {
		if got := poeDisplayName(input); got != want {
			t.Errorf("%q => %q want %q", input, got, want)
		}
	}
	if got := poeBotHandle("gpt-6-luna", "GPT-6 Luna"); got != "GPT-6-Luna" {
		t.Fatal(got)
	}
}
