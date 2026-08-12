# Running the server locally

`cmd/server` is the real HTTP entry point (`main.go` at the repo root is a separate, router-only demo — leave it alone). It has never been tested against a live vendor API from inside a Claude Code on the web session: that session's outbound network policy blocks arbitrary external hosts (confirmed for both `api.openai.com` and `openrouter.ai` — a 403 from the session's own egress proxy, not from either vendor). Testing against a real API has to happen on a machine without that restriction — most likely yours.

**Confirmed working (2026-08-09):** a local run reached `handleChat` → `handle` → `classify` → OpenRouter and got real HTTP responses back (429 rate-limit, 401 on a stale key in a second terminal) — proof the whole wiring (auth header, request format, routing) is correct. No successful generated response has been captured yet; that's still open.

The server calls vendors through **OpenRouter** (`provider.OpenRouterClient`, `https://openrouter.ai/api/v1`), matching the product's original "Model access: OpenRouter at launch" decision (see README). OpenRouter model IDs are `vendor/model-name` (e.g. `google/gemini-3.5-flash-lite`), not a vendor's own bare model name — get this string wrong and every call 404s.

## 1. Start everything

`cmd/server` requires a real Postgres and Redis (see README's "Current task: local dev infrastructure") — the `InMemory*` stand-ins are gone. All three — Postgres, Redis, and the server itself — run in Docker via `docker-compose.yml` at the repo root; there's no separate `go run` step for normal use.

Prereqs, once per machine:
- A swap file if the host is memory-constrained (this repo's dev host is 2 GB RAM — see README point 1 under "Current task").
- Docker + the `docker compose` plugin (`docker compose version` should print something; if it doesn't, install the plugin — see Docker's docs).

```bash
cp .env.example .env    # fill in real POSTGRES_*/REDIS_* values (anything works locally) and OPENROUTER_API_KEY
docker compose up -d --build
docker compose ps        # wait until all three services show "healthy"
```

`server`'s `Dockerfile` builds `cmd/server` (not the root `main.go` demo) and bakes in `configs/` and `prompts/`. `docker-compose.yml` overrides `POSTGRES_HOST`/`REDIS_HOST` to the in-network service names (`postgres`/`redis`) regardless of what `.env` has — `.env`'s `127.0.0.1` defaults are for running `cmd/server` on the host instead (see "Running without Docker" below), not for the containerized `server` service.

`cmd/server` applies every pending migration under `db/migrations/` automatically on startup (see `db.Migrate`) — there is no separate manual migration step in the normal case. To inspect the database by hand anyway:

```bash
docker exec -it neochat-postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"
docker exec -it neochat-redis redis-cli -a "$REDIS_PASSWORD"
```

**Data persistence footgun:** `docker compose down` keeps the named volumes (`neochat_pg_data`, `neochat_redis_data`) — your data survives. `docker compose down -v` deletes them — everything is gone. Only use `-v` when you actually want a clean slate.

## 2. Set the required environment variables

All of these go in `.env` (gitignored) — `cmd/server` loads it automatically on startup via `internal/envfile`, and a real environment variable always takes priority over one from the file.

| Variable | Meaning |
|---|---|
| `OPENROUTER_API_KEY` | Required. No key, no real vendor calls — `cmd/server` refuses to start without it. |
| `CLASSIFIER_API_MODEL_ID` | Optional, defaults to `google/gemini-3.5-flash-lite` (the model `prompts/classifier_system_prompt.md` was validated against). Override for a different OpenRouter slug. |
| `MODERATION_API_MODEL_ID` | Optional, defaults to `openai/gpt-oss-120b` (the model `docs/unit-economics.md` assumed for moderation). Override for a different OpenRouter slug. |
| `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` | Required. Must match what `docker-compose.yml` started Postgres with. |
| `POSTGRES_HOST` / `POSTGRES_PORT` | Optional, default to `127.0.0.1` / `5432` (docker-compose's published address). |
| `REDIS_PASSWORD` | Required. Must match what `docker-compose.yml` started Redis with. |
| `REDIS_HOST` / `REDIS_PORT` | Optional, default to `127.0.0.1` / `6379`. |
| `IDEMPOTENCY_TTL` | Optional, defaults to `24h` (Go duration syntax). How long a completed `/chat` response stays replayable by `idempotency_key` in Redis. |
| `ADDR` | Optional, defaults to `:8080`. |

**Never commit a real key.** `.env` is gitignored specifically so `OPENROUTER_API_KEY` and the Postgres/Redis credentials never end up in git — don't paste real values into `.env.example` or any other tracked file.

## 3. It's already running

Step 1's `docker compose up -d --build` already started `cmd/server` — there's nothing further to run. Check the logs:

```bash
docker compose logs -f server
```

On success you'll see one `db: applied migration ...` log line per migration file the first time (already-applied migrations are silently skipped on later restarts), then `listening on :8080`. The container's `HEALTHCHECK` hits `GET /health`; `docker compose ps` shows `healthy` once that starts succeeding.

Changed a `.go` file, `configs/*.json`, or `prompts/*.md`? Rebuild and recreate just that service:

```bash
docker compose up -d --build server
```

### Running without Docker

For a tight edit/run loop without a rebuild each time, run `cmd/server` directly on the host against the same containerized Postgres/Redis (`docker compose up -d postgres redis` on their own, skip `server`):

```bash
go run ./cmd/server
```

This picks up `.env`'s `127.0.0.1` host defaults, which is exactly right here since the host process reaches the containers through their published ports, not the compose-internal network.

## 4. Send a test request

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
  "conversation_id": "...",
  "selected_model_id": "...",
  "selected_mode": "instant | thinking | max",
  "reason": "...",
  "estimated_cost_usd": 0.0,
  "actual_cost_usd": 0.0,
  "response_text": "..."
}
```

Every request now also runs through Layer 1 moderation (`moderation/`, see README "Moderation") concurrently with classification. If it flags the message, the response instead looks like this — no model is ever selected or called:

```json
{
  "conversation_id": "...",
  "selected_model_id": "",
  "selected_mode": "",
  "reason": "",
  "estimated_cost_usd": 0.0,
  "actual_cost_usd": 0.0,
  "response_text": "This message was blocked because it violates our usage policies.",
  "blocked": true
}
```

To continue the same thread instead of starting a new one each time, pass the `conversation_id` the previous response returned:

```bash
curl -s localhost:8080/chat -X POST -d '{
  "user_id": "test-user",
  "plan_id": "pro",
  "conversation_id": "PASTE_THE_ONE_FROM_THE_LAST_RESPONSE",
  "message": "а теперь объясни то же самое проще",
  "requested_mode": "auto",
  "estimated_context_tokens": 200
}' | python3 -m json.tool
```

Leaving `conversation_id` out (or empty) always starts a brand new conversation — there's no way to "continue the most recent one implicitly," the client has to track and pass the ID itself.

`plan_id` must be one of `configs/plans.json`'s `plan_id` values (`pro`, `pro_plus`, `max`).

## What to watch for

- **`dial tcp 127.0.0.1:5432: connect: connection refused`** (or `:6379` for Redis) — Postgres/Redis containers aren't up yet, or aren't healthy yet. Run `docker compose ps` and wait for both to show `healthy`; if either is missing entirely, `docker compose up -d` from the repo root first.
- **`db: POSTGRES_USER is required` / `db: REDIS_PASSWORD is required`** — `.env` is missing, in the wrong directory (must be repo root, next to `docker-compose.yml`), or a real exported env var is empty-stringed and shadowing what `.env` would have set. Check `cp .env.example .env` was actually done and filled in.
- **`no provider.Client configured for provider "..."`** — the router selected a model from a catalog provider with no wired-up client. Shouldn't happen anymore: `cmd/server/main.go`'s `Generators` map now covers all five catalog providers (`anthropic`, `openai`, `google`, `moonshot`, `deepseek`) through the same `OpenRouterClient`. If you see this, either a new catalog entry added a provider tag not yet in that map, or there's a typo between the two.
- **A 404 / "No endpoints found" error from OpenRouter** — `api_model_id` doesn't match a real OpenRouter slug; double-check the exact string on openrouter.ai's model page (copy-paste, don't retype). Every `configs/models.json` entry now sets `api_model_id` explicitly, sourced via search (this session can't reach `openrouter.ai` directly to fetch pages itself — see README "Status") rather than a live confirmed call per model, so a 404 here likely means one of those looked-up slugs is stale or wrong and needs a real check.
- **429** — rate limit or exhausted free-tier quota on the key, not a bug here.
- **401** — bad/expired key; double check the exact value in your shell, not a leftover from an earlier export.
- **The classifier reply fails to parse as JSON** — `classifier.Classify` already strips a wrapping ` ```json ` fence, but a genuinely different failure (the model refusing, adding prose, etc.) will surface as a clear parse error with the raw reply included — that's real signal about how the model behaves with this prompt through OpenRouter specifically, not necessarily a bug.
- **`moderate: ...` error, every request fails** — the moderation model call itself failed (bad `MODERATION_API_MODEL_ID`, rate limit, JSON parse failure — same failure modes as the classifier, just in `moderation.Moderate`). Moderation fails closed on purpose (see README "Moderation"), so this takes down `/chat` entirely rather than letting requests through unmoderated — check the error for which of those it actually is.
- **Every request comes back `"blocked": true`** — either genuinely correct (you're testing with a phrase that should trip a policy category) or the moderation model is being overly aggressive; check `docs/unit-economics.md`'s cost assumption still matches whichever model `MODERATION_API_MODEL_ID` resolves to, and see the flagged reason in the server log (`server: /chat blocked by moderation ...`) — it's logged server-side even though the client only sees the generic ToS message.

## Confirming the Postgres/Redis wiring specifically

After a request or two through step 4 above:

```bash
# moderation.BlockLog -- send a request that should trip a policy category first
docker exec -it neochat-postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c 'select * from moderation_block_log;'

# limits.SpendStore -- after any non-blocked request
docker exec -it neochat-redis redis-cli -a "$REDIS_PASSWORD" keys 'spend:*'

# conversation.Store -- two requests sharing the same conversation_id should show two rows, in order
docker exec -it neochat-postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c 'select role, content, model_id, created_at from conversation_messages order by id;'

# costlog.Store -- one row per completed generation
docker exec -it neochat-postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c 'select model_id, mode, cost_usd from cost_log order by id;'

# idempotency.Store -- resend the exact same request body with the same idempotency_key; the second
# response should be byte-identical to the first, and cost_log should NOT get a second row for it
docker exec -it neochat-redis redis-cli -a "$REDIS_PASSWORD" keys 'idem:*'
```

Restart survival: `docker compose restart postgres redis` (no `-v`), then re-run the `conversation_messages` query above for the same `conversation_id` — the rows from before the restart should still be there (named volumes, not `down -v`).

## After testing

Rotate any key that was ever pasted anywhere outside your own shell (chat, a shared doc, etc.) — treat a key that touched a chat transcript as compromised, regardless of whether the test succeeded.
