# Running the server locally

`cmd/server` is the real HTTP entry point (`main.go` at the repo root is a separate, router-only demo — leave it alone). It has never been tested against a live vendor API from inside a Claude Code on the web session: that session's outbound network policy blocks arbitrary external hosts (confirmed for both `api.openai.com` and `openrouter.ai` — a 403 from the session's own egress proxy, not from either vendor). Testing against a real API has to happen on a machine without that restriction — most likely yours.

**Confirmed working (2026-08-09):** a local run reached `handleChat` → `handle` → `classify` → OpenRouter and got real HTTP responses back (429 rate-limit, 401 on a stale key in a second terminal) — proof the whole wiring (auth header, request format, routing) is correct. No successful generated response has been captured yet; that's still open.

The server calls vendors through **OpenRouter** (`provider.OpenRouterClient`, `https://openrouter.ai/api/v1`), matching the product's original "Model access: OpenRouter at launch" decision (see README). OpenRouter model IDs are `vendor/model-name` (e.g. `google/gemini-3.5-flash-lite`), not a vendor's own bare model name — get this string wrong and every call 404s.

## 1. Set the required environment variables

| Variable | Meaning |
|---|---|
| `OPENROUTER_API_KEY` | Required. No key, no real vendor calls — `cmd/server` refuses to start without it. |
| `CLASSIFIER_API_MODEL_ID` | Optional, defaults to `google/gemini-3.5-flash-lite` (the model `prompts/classifier_system_prompt.md` was validated against). Override for a different OpenRouter slug. |
| `ADDR` | Optional, defaults to `:8080`. |

**Never commit a real key.** Set it in your shell for the one command, not in a file that gets `git add`ed:

```bash
export OPENROUTER_API_KEY="sk-or-..."
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

## What to watch for

- **`no provider.Client configured for provider "..."`** — the router selected a model from a catalog provider with no wired-up client. Shouldn't happen anymore: `cmd/server/main.go`'s `Generators` map now covers all five catalog providers (`anthropic`, `openai`, `google`, `moonshot`, `deepseek`) through the same `OpenRouterClient`. If you see this, either a new catalog entry added a provider tag not yet in that map, or there's a typo between the two.
- **A 404 / "No endpoints found" error from OpenRouter** — `api_model_id` doesn't match a real OpenRouter slug; double-check the exact string on openrouter.ai's model page (copy-paste, don't retype). Every `configs/models.json` entry now sets `api_model_id` explicitly, sourced via search (this session can't reach `openrouter.ai` directly to fetch pages itself — see README "Status") rather than a live confirmed call per model, so a 404 here likely means one of those looked-up slugs is stale or wrong and needs a real check.
- **429** — rate limit or exhausted free-tier quota on the key, not a bug here.
- **401** — bad/expired key; double check the exact value in your shell, not a leftover from an earlier export.
- **The classifier reply fails to parse as JSON** — `classifier.Classify` already strips a wrapping ` ```json ` fence, but a genuinely different failure (the model refusing, adding prose, etc.) will surface as a clear parse error with the raw reply included — that's real signal about how the model behaves with this prompt through OpenRouter specifically, not necessarily a bug.

## After testing

Rotate any key that was ever pasted anywhere outside your own shell (chat, a shared doc, etc.) — treat a key that touched a chat transcript as compromised, regardless of whether the test succeeded.
