# Running the server locally

`cmd/server` is the HTTP entry point. All AI calls use Poe; see
[the Poe migration guide](poe-migration.md) for transports, model/bot names,
Unlisted bot entries, prompt-cache telemetry, media and point accounting.

The existing chat features remain: personas, streaming, terminal/desktop/MCP
tools, inline script images, files, web search, chat history, summaries,
regeneration, projects and settings sync. The root `main.go` is a separate
router demo. Live Poe calls and billing require your own Poe key and are not
verified by the offline test suite.

## 1. Start everything

`cmd/server` requires a real Postgres and Redis (see README's "Current task: local dev infrastructure") — the `InMemory*` stand-ins are gone. All three — Postgres, Redis, and the server itself — run in Docker via `docker-compose.yml` at the repo root; there's no separate `go run` step for normal use.

Prereqs, once per machine:
- A swap file if the host is memory-constrained (this repo's dev host is 2 GB RAM — see README point 1 under "Current task").
- Docker + the `docker compose` plugin (`docker compose version` should print something; if it doesn't, install the plugin — see Docker's docs).

```bash
cp .env.example .env    # fill in real POSTGRES_*/REDIS_* values (anything works locally); POE_API_KEY is optional
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
| `POE_API_KEY` | Optional server-side fallback key, used only for calls that arrive without a user key. Leave empty to require every user to bring their own (Settings → Account). |
| `ROUTER_ENABLED` | Optional. Unset/anything but `true` = router disabled, Manual-only direct mode (see above). |
| `MODEL_CATALOG_LIVE` | Optional. `false` keeps the checked-in Poe snapshot; otherwise the public list refreshes hourly. |
| `MAX_OUTPUT_TOKENS` | Optional, defaults to `16000` (range 1..16000). Hard ceiling on output tokens per generation call. |
| `CLASSIFIER_API_MODEL_ID` | Optional, defaults to `gemini-3.5-flash-lite` (the model `prompts/classifier_system_prompt.md` was validated against). Only used with the router enabled, and as the summarizer default. |
| `MODERATION_API_MODEL_ID` | Optional, defaults to `gpt-oss-120b`. Only used with the router enabled. |
| `CLASSIFIER_COST_INPUT_PER_MTOK` / `CLASSIFIER_COST_OUTPUT_PER_MTOK` | Optional, default `0.303` / `2.5253` (matches the `CLASSIFIER_API_MODEL_ID` default). Prices the classifier's `cost_log` entries -- override alongside `CLASSIFIER_API_MODEL_ID` if you change it, or logged cost keeps pricing the old model. |
| `MODERATION_COST_INPUT_PER_MTOK` / `MODERATION_COST_OUTPUT_PER_MTOK` | Optional, default `0.1515` / `0.6061` (matches the `MODERATION_API_MODEL_ID` default). Same caveat as the classifier rates above. |
| `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` | Required. Must match what `docker-compose.yml` started Postgres with. |
| `POSTGRES_HOST` / `POSTGRES_PORT` | Optional, default to `127.0.0.1` / `5432` (docker-compose's published address). |
| `REDIS_PASSWORD` | Required. Must match what `docker-compose.yml` started Redis with. |
| `REDIS_HOST` / `REDIS_PORT` | Optional, default to `127.0.0.1` / `6379`. |
| `IDEMPOTENCY_TTL` | Optional, defaults to `24h` (Go duration syntax). How long a completed `/chat` response stays replayable by `idempotency_key` in Redis. |
| `ADDR` | Optional, defaults to `:8080`. |

**Never commit a real key.** `.env` is gitignored specifically so `POE_API_KEY` and the Postgres/Redis credentials never end up in git — don't paste real values into `.env.example` or any other tracked file.

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

## 4. Mint yourself an API key

`POST /chat`/`POST /chat/stream` require an `Authorization: Bearer <api-key>` header as of 2026-08-14 (see README "Authentication", `audit.md` finding #1) — `user_id`/`plan_id` come from this key server-side, not from the request body anymore. There's no signup flow yet, so `cmd/issuekey` is the entire way to get one: it writes a row straight into `api_keys` (the same Postgres this whole guide has been setting up) and prints the raw token once.

```bash
go run ./cmd/issuekey -user_id=test-user -plan_id=pro
```

`-plan_id` must be one of `configs/plans.json`'s `plan_id` values (`pro`, `pro_plus`, `max`) — `issuekey` validates this itself and refuses to mint a key for anything else. Copy the printed token; it's shown exactly once and isn't recoverable from the database afterward (only its hash is stored).

```bash
export NEOCHAT_API_KEY=nc_...   # paste what issuekey printed
```

## 5. Send a test request

```bash
export POE_KEY=YOUR_POE_KEY   # your own Poe key
curl -s localhost:8080/chat -X POST \
  -H "Authorization: Bearer $NEOCHAT_API_KEY" \
  -H "X-Provider-Key: $POE_KEY" \
  -d '{
  "message": "привет",
  "requested_mode": "manual",
  "manual_model_id": "deepseek-v4.1-flash",
  "reasoning_effort": "low",
  "persona": "Default",
  "instructions": "Answer briefly."
}' | python3 -m json.tool
```

With the router disabled (the default), use `requested_mode: manual` and a
model ID from `GET /models`. Effort values are mapped to the selected bot's
advertised parameters. Unlisted curated bots remain selectable by their exact Poe handles.
The `*_cost_usd` response fields are internal estimates; Poe charges points.

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

With `ROUTER_ENABLED=true`, every request also runs through Layer 1 moderation (`moderation/`, see README "Moderation") concurrently with classification. If it flags the message, the response instead looks like this — no model is ever selected or called:

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
curl -s localhost:8080/chat -X POST \
  -H "Authorization: Bearer $NEOCHAT_API_KEY" \
  -H "X-Provider-Key: $POE_KEY" \
  -d '{
  "conversation_id": "PASTE_THE_ONE_FROM_THE_LAST_RESPONSE",
  "message": "а теперь объясни то же самое проще",
  "requested_mode": "manual",
  "manual_model_id": "deepseek-v4.1-flash"
}' | python3 -m json.tool
```

Leaving `conversation_id` out (or empty) always starts a brand new conversation — there's no way to "continue the most recent one implicitly," the client has to track and pass the ID itself.

A request with no `Authorization` header, or a token `issuekey` never printed, gets a 401 before any of the above runs. `user_id`/`plan_id` in the JSON body (if you leave them in out of habit) are silently ignored either way -- both now come exclusively from the key.

## What to watch for

- **`dial tcp 127.0.0.1:5432: connect: connection refused`** (or `:6379` for Redis) — Postgres/Redis containers aren't up yet, or aren't healthy yet. Run `docker compose ps` and wait for both to show `healthy`; if either is missing entirely, `docker compose up -d` from the repo root first.
- **`db: POSTGRES_USER is required` / `db: REDIS_PASSWORD is required`** — `.env` is missing, in the wrong directory (must be repo root, next to `docker-compose.yml`), or a real exported env var is empty-stringed and shadowing what `.env` would have set. Check `cp .env.example .env` was actually done and filled in.
- **`no provider.Client configured for provider "..."`** — the router selected a model from a catalog provider with no wired-up client. Shouldn't happen anymore: `cmd/server/main.go`'s `Generators` map now covers all five catalog providers (`anthropic`, `openai`, `google`, `moonshot`, `deepseek`) (plus `spacexai`) through the same `PoeClient`. If you see this, either a new catalog entry added a provider tag not yet in that map, or there's a typo between the two.
- **"Poe: ... (HTTP 404)"** — the bot name is unknown or unavailable to this key. Check the Poe bot URL and `https://api.poe.com/v1/models`; unlisted bots may still be callable.
- **"Poe rejected your API key"** (401/403) or **"balance is too low"** (402) — the user's own key/balance; shown to them verbatim since it's their account.
- **429** — rate limit on the user's Poe key, not a bug here.
- **401 straight back from `curl localhost:8080/chat`, instant, no server-side classify/moderate log line** — this is neochat's own auth check (step 4 above), not OpenRouter: missing `Authorization` header, or a token `cmd/issuekey` never printed for this database. Re-run `issuekey` and re-export `NEOCHAT_API_KEY` if in doubt -- a stale value from an earlier `.env`/database reset looks identical to a typo.
- **"Add your Poe API key"** — the request had no `X-Provider-Key` header (key not saved in Settings → Account) (Manual mode requires a user key).
- **The classifier reply fails to parse as JSON** — `classifier.Classify` already strips a wrapping ` ```json ` fence, but a genuinely different failure (the model refusing, adding prose, etc.) will surface as a clear parse error with the raw reply included — that's real signal about how the model behaves with this prompt through Poe specifically, not necessarily a bug.
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

## Running the integration tests

Most of the suite runs with no dependencies (`go test ./...`). The tests
that exercise the real Postgres/Redis-backed stores instead of the
`InMemory*` stand-ins are behind a build tag, so they only run when asked
for — and they skip themselves (rather than fail) if the environment
variables they need aren't exported:

```bash
docker compose up -d --wait postgres redis
set -a && source .env && set +a
go test -tags=integration ./...
```

Watch for `SKIP` in the output: it means the `POSTGRES_*`/`REDIS_*`
variables didn't reach the test process, not that everything passed.

CI runs exactly this on every push and pull request (`.github/workflows/ci.yml`),
against the same `docker-compose.yml` services, and fails the run if any
of these tests skip — so a broken store implementation can't reach `main`
just because nobody remembered to run the tagged suite by hand.

## Editing the landing page

Signed-out visitors see the landing page instead of the chat. It is a
small React app in `web/landing/` (including the vendored react-bits
`AeroShards` background, which needs WebGPU and falls back to a flat
background without it). Its build output in `server/assets/landing/` is
committed and embedded into the Go binary, so `go build` and the Dockerfile
never need Node. After changing anything under `web/landing/src`:

```bash
cd web/landing
npm ci
npm run build   # rewrites server/assets/landing/landing.{js,css}
```

Then bump the `?v=` query on both `/assets/landing/...` URLs in
`server/frontend.html` so browsers drop the cached copy. CI rebuilds the
bundle and fails if the committed output is stale.

## After testing

Rotate any key that was ever pasted anywhere outside your own shell (chat, a shared doc, etc.) — treat a key that touched a chat transcript as compromised, regardless of whether the test succeeded.
