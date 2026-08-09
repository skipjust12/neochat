# Running the server locally

`cmd/server` is the real HTTP entry point (`main.go` at the repo root is a separate, router-only demo — leave it alone). It has never been tested against a live vendor API from inside a Claude Code on the web session: that session's outbound network policy blocks arbitrary external hosts (confirmed for both `api.openai.com` and `openrouter.ai` — a 403 from the session's own egress proxy, not from either vendor). Testing against a real API has to happen on a machine without that restriction — most likely yours.

The server calls vendors through **OpenRouter** (`provider.OpenRouterClient`, `https://openrouter.ai/api/v1`), matching the product's original "Model access: OpenRouter at launch" decision (see README). OpenRouter model IDs are `vendor/model-name` (e.g. `google/gemma-4-31b-it:free`), not a vendor's own bare model name — get this string wrong and every call 404s.

## 0. Temporary test entry: `gemma-4-31b-it:free`

`configs/models.json` currently has a **temporary** entry added just to prove the pipeline works end to end on a free-tier OpenRouter key:

```json
{
  "id": "gemma-4-31b-it:free",
  "api_model_id": "google/gemma-4-31b-it:free",
  "provider": "google",
  "modes": ["instant"],
  "cost_input_per_mtok": 0.0,
  "cost_output_per_mtok": 0.0,
  ...
}
```

Cost is set to $0/Mtok on purpose so `scoreAndPick` (cheapest-in-tier wins) picks it over every other instant-tier model automatically — a plain `"requested_mode": "auto"` request with a simple message like "привет" should land on it without needing `manual_model_id`. `cmd/server/main.go`'s `Generators` map only wires a real client for `provider: "google"` right now (this entry's provider) — routing to anything else will fail cleanly with `no provider.Client configured for provider "..."` until either that catalog entry gets a verified OpenRouter slug or the map is extended.

**Remove this entry (and the `Generators` map comment referencing it) once the pipeline is confirmed working** — it's not a real catalog model, just a known-good target for the first live test.

## 1. Set the required environment variables

| Variable | Meaning |
|---|---|
| `OPENROUTER_API_KEY` | Required. No key, no real vendor calls — `cmd/server` refuses to start without it. |
| `CLASSIFIER_API_MODEL_ID` | Required. The exact OpenRouter model slug to classify with — for a first test, reuse `google/gemma-4-31b-it:free` here too, since it's the only confirmed-available model on a free-trial key. |
| `ADDR` | Optional, defaults to `:8080`. |

**Never commit a real key.** Set it in your shell for the one command, not in a file that gets `git add`ed:

```bash
export OPENROUTER_API_KEY="sk-or-..."
export CLASSIFIER_API_MODEL_ID="google/gemma-4-31b-it:free"
```

## 2. Run it

```bash
go run ./cmd/server
```

## 3. Send a test request

```bash
curl -s localhost:8080/chat -X POST -d '{
  "user_id": "test-user",
  "plan_id": "pro",
  "message": "привет",
  "requested_mode": "auto",
  "estimated_context_tokens": 200
}' | python3 -m json.tool
```

To bypass the classifier/auto-mode entirely and force Gemma regardless of what gets classified (useful for isolating "does the plumbing work at all" from "does auto-routing pick sensibly"):

```bash
curl -s localhost:8080/chat -X POST -d '{
  "user_id": "test-user",
  "plan_id": "pro",
  "message": "привет",
  "requested_mode": "manual",
  "manual_model_id": "gemma-4-31b-it:free"
}' | python3 -m json.tool
```

Expected response shape:

```json
{
  "selected_model_id": "gemma-4-31b-it:free",
  "selected_mode": "instant",
  "reason": "...",
  "estimated_cost_usd": 0.0,
  "actual_cost_usd": 0.0,
  "response_text": "..."
}
```

`plan_id` must be one of `configs/plans.json`'s `plan_id` values (`pro`, `pro_plus`, `max`).

## What to watch for on the first real run

- **`no provider.Client configured for provider "..."`** — the router selected a model tagged with a catalog provider other than `google` (only `google` has a wired-up client right now, see `cmd/server/main.go`'s `Generators` map and step 0 above). Use the manual-mode request above to pin it to Gemma, or this is the point where a second `provider.Client`/`Generators` entry needs adding.
- **A 404 / "No endpoints found" error from OpenRouter** — `api_model_id` doesn't match a real OpenRouter slug; double-check the exact string on openrouter.ai's model page (copy-paste, don't retype).
- **The classifier reply fails to parse as JSON** — `classifier.Classify` already strips a wrapping ` ```json ` fence, but a genuinely different failure (the model refusing, adding prose, etc.) will surface as a clear parse error with the raw reply included — that's real signal about how the model your key has access to behaves with this prompt, not necessarily a bug.

## After testing

Rotate the key you used for this test if it was ever pasted anywhere outside your own shell (chat, a shared doc, etc.) — treat any key that touched a chat transcript as compromised, regardless of whether the test succeeded.
