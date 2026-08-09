# Running the server locally

`cmd/server` is the real HTTP entry point (`main.go` at the repo root is a separate, router-only demo — leave it alone). It has never been tested against a live vendor API from inside a Claude Code on the web session: that session's outbound network policy blocks arbitrary external hosts (confirmed for both `api.openai.com` and `openrouter.ai` — a 403 from the session's own egress proxy, not from either vendor). Testing against a real API has to happen on a machine without that restriction — most likely yours.

## 1. Find your model's real API name

The catalog (`configs/models.json`) uses forward-looking internal names like `gpt-5.6-luna` for cataloging purposes. There is no guarantee that string is what OpenAI's API actually expects — a free-trial account in particular likely only has access to a small set of base models. Check what your key can actually call before assuming the catalog name works:

```bash
curl https://api.openai.com/v1/models -H "Authorization: Bearer $OPENAI_API_KEY" | python3 -m json.tool
```

If the real model name differs from the catalog id, don't hand-edit `models.json`'s `id` (that's the internal name the router's tier/pricing logic keys off). Instead set `api_model_id` on that catalog entry — `router.Model.ResolveAPIModelID()` returns it in preference to `id`, and that's what `provider.Client.Generate` is actually called with:

```json
{
  "id": "gpt-5.6-luna",
  "api_model_id": "gpt-4o-mini",
  "provider": "openai",
  ...
}
```

## 2. Set the required environment variables

| Variable | Meaning |
|---|---|
| `OPENAI_API_KEY` | Required. No key, no real vendor calls — `cmd/server` refuses to start without it. |
| `CLASSIFIER_API_MODEL_ID` | Required. The exact vendor-side model string to classify with (same "check what your key can actually call" caveat as above — this does not have to be the same model as generation). |
| `ADDR` | Optional, defaults to `:8080`. |

**Never commit a real key.** Set it in your shell for the one command, not in a file that gets `git add`ed:

```bash
export OPENAI_API_KEY="sk-..."
export CLASSIFIER_API_MODEL_ID="gpt-4o-mini"
```

## 3. Run it

```bash
go run ./cmd/server
```

## 4. Send a test request

```bash
curl -s localhost:8080/chat -X POST -d '{
  "user_id": "test-user",
  "plan_id": "pro",
  "message": "write a haiku about databases",
  "requested_mode": "auto",
  "estimated_context_tokens": 500
}' | python3 -m json.tool
```

Expected response shape:

```json
{
  "selected_model_id": "...",
  "selected_mode": "instant | thinking | max",
  "reason": "...",
  "estimated_cost_usd": 0.0,
  "actual_cost_usd": 0.0,
  "response_text": "..."
}
```

`plan_id` must be one of `configs/plans.json`'s `plan_id` values (`pro`, `pro_plus`, `max`).

## What to watch for on the first real run

- **`no provider.Client configured for provider "..."`** — the router selected a model from a catalog provider other than `openai` (only `openai` has a wired-up client right now, see `cmd/server/main.go`'s `Generators` map). Either restrict testing to OpenAI-catalog models (`requested_mode: "manual"` + a `manual_model_id` like `gpt-5.6-luna`/`gpt-oss-120b`), or this is the point where a second `provider.Client` needs writing.
- **A 404/`model does not exist` error from OpenAI** — `api_model_id` doesn't match what your key can actually call; go back to step 1.
- **The classifier reply fails to parse as JSON** — `classifier.Classify` already strips a wrapping ` ```json ` fence, but a genuinely different failure (the model refusing, adding prose, etc.) will surface as a clear parse error with the raw reply included — that's real signal about how the base model your key has access to behaves with this prompt, not necessarily a bug.

## After testing

Rotate the key you used for this test if it was ever pasted anywhere outside your own shell (chat, a shared doc, etc.) — treat any key that touched a chat transcript as compromised, regardless of whether the test succeeded.
