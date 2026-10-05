# Running the server locally

`cmd/server` is the real HTTP entry point (`main.go` at the repo root is a separate, router-only demo — leave it alone). It has never been tested against a live vendor API from inside a Claude Code on the web session: that session's outbound network policy blocks arbitrary external hosts (confirmed for both `api.openai.com` and `openrouter.ai` — a 403 from the session's own egress proxy, not from either vendor). Testing against a real API has to happen on a machine without that restriction — most likely yours.

**Confirmed working (2026-08-09):** a local run reached `handleChat` → `handle` → `classify` → OpenRouter and got real HTTP responses back (429 rate-limit, 401 on a stale key in a second terminal) — proof the whole wiring (auth header, request format, routing) is correct. No successful generated response has been captured yet; that's still open.

**Provider: Polza AI (since 2026-10).** OpenRouter is gone. The server calls every model through **Polza AI** (`provider.PolzaClient`, `https://polza.ai/api/v1`, OpenAI-compatible). Each user pastes their own Polza key (`pza_...`) in Settings → Account; the browser sends it with every chat request as `X-Provider-Key`, the server attaches it to that one vendor call and never stores it. Polza model IDs are `vendor/model-name` (e.g. `anthropic/claude-opus-5.5`); `configs/models.json` was generated from Polza's public catalog (`GET https://polza.ai/api/v1/models`, no auth needed), prices converted from RUB at Polza's own rate (1170.68 ₽ = $10 for Fable 5.1).

**Router disabled.** By default (`ROUTER_ENABLED` unset) only Manual mode works: Auto/Instant/Thinking/Max requests are rejected with "Router disabled", and classify/moderate are skipped entirely — the request goes straight to the chosen model with the chosen reasoning effort, the selected tone (persona prompt from `prompts/<Name>.md`) and the user's custom instructions. `ROUTER_ENABLED=true` brings the full classify → moderate → route pipeline back.

**File attachments.** The composer's + menu (or paste / drag-and-drop) uploads each file to `POST /files` (multipart, field `file`, bearer auth) as soon as it's picked; the chat request then lists the returned ids in `"attachments"`. The server decides the type from the bytes, not the name: images (PNG/JPEG/GIF/WebP, up to 20 MB uploaded), documents (PDF/DOCX, max 20 MB), videos (MP4/MOV/WebM/3GP, max 50 MB; 60 MB of files per message) and UTF-8 text/code (max 512 KB). Images are stored at most 1568 px on the long side, the size models actually use: the browser re-encodes every image to WebP 0.82 before upload (a phone photo ends up ~100-300 KB), and the server re-encodes anything that still arrives bigger (JPEG q82, or PNG with transparency) and rejects images over 40 megapixels. Images, documents and videos go to Polza as `image_url` / `file` / `video_url` parts, so the chosen model has to support them (`input_modalities` in `configs/models.json`; only Gemini models watch videos); text files are inlined and work everywhere. Videos play in the chat: the page fetches the file and plays it from a `blob:` URL. Files live in the `attachments` table and are re-sent on later turns (up to 30 MB of history). Deleting a chat deletes its files in the same transaction; an hourly sweep (`server.RunCleanup`) removes files picked but never sent for 24 hours and any file whose chat no longer exists. Postgres reuses the freed space for new rows; the table file itself only shrinks after `VACUUM FULL attachments`. Files are not available in incognito chats.

**Replies survive dropped connections.** A chat request with an `idempotency_key` (the web UI always sends one) runs as a job detached from its HTTP request (`server/streamjob.go`), so a phone that puts the app in the background or a network that drops doesn't stop the answer. Each SSE connection starts with `event: stream` (`{"stream_id": …}`) and numbers every later event (`id:`); the server sends a `: ping` comment every 15 seconds. `GET /chat/stream/{stream_id}?after=N` replays what came after event N and follows the rest live; a POST retried with the same idempotency key (and `Last-Event-ID`) attaches to the running job instead of starting again. Since leaving no longer stops such a reply, Stop is `POST /chat/stop` with `{"stream_id"}` or `{"idempotency_key"}`: it answers once the partial reply is stored, and a Stop that overtakes its own request still stops it. Requests without a key keep the old contract (leaving stops them). Jobs live in the server's memory and stay replayable for 15 minutes after they finish (at most 4 finished ones per user); after that the stored chat has the answer. Shutdown lets running jobs finish within the grace period, then stops the rest. The UI retries a dropped stream up to five times ("Streaming interrupted, retrying (n/5)", 0.8–8 s apart, waiting while the app is in the background), treats 45 seconds of silence as a dropped connection, and notes the stream in `localStorage` (`neochat-active-stream`, saved chats only) so a page the phone reloaded rejoins the reply before it loads the chat.

**Thinking and live replies on every device.** Models that share their reasoning (Polza's `delta.reasoning`, or the text/summary parts of `reasoning_details`) stream it as `reasoning` events; the UI keeps it out of sight until you tap what the model is doing ("Pondering", "Searching the web") or, later, "Thought for N seconds". It is stored with the response version (`reasoning`, first 16k characters). `GET /events` is one long-lived SSE stream per open app (`started` / `finished`, with the stream id, chat id and the message asked; a comment every 25 s; incognito replies are never announced). A device showing that chat follows the reply through `GET /chat/stream/{stream_id}`, any device can stop it, and a chat another device just started shows up in the sidebar with a live dot before it is stored. History and the chat list no longer take the active-request slot, so they open while a reply streams. Shutdown closes the `/events` streams (clients reconnect).

**Sketch and low balance alerts.** "+" → Sketch opens a drawing board in the page (no server part): freehand pen and eraser, shapes that stay movable and resizable, six colors and undo; Confirm exports a PNG and hands it to the attachment uploader like a picked photo, so it is sent with the next message. The balance behind the chat key (`GET /account/balance`) is checked when the app opens, 2.5 s after each answer (also answers finished on another device), when the key changes and on return after 10 minutes; the two thresholds from Settings → Usage (`balanceWarnAt`, `balanceLowAt` in the synced preferences) decide which note shows above the message box.

**Model list follows Polza.** `configs/models.json` is the curated catalog; `modelcatalog.Live` merges Polza's public list (`GET https://polza.ai/api/v1/models`, no key) into it. A chat or image model from one of the picker's six vendors that Polza added after the newest curated model joins the catalog: one continuing a known line (same name with a higher version, e.g. Claude Haiku 5.5 after Claude Haiku 4.5) takes that line's place, inheriting its tier, description and routing data, and the model it replaces becomes `legacy`; one starting a new line goes to the top of its vendor's list with a tier guessed from its price. Prices, context window, inputs, tool calling and reasoning come from Polza's entry (rubles at 117.068 per dollar). Added models are Manual-only; variants (`-exp`, `search`, `customtools`, …) and other vendors are skipped, and a list missing half the curated models or carrying more than 30 new ones is refused. The server fetches it at startup and again when someone opens the app (`GET /models`, public) and it's over 10 minutes old; Settings → General → Refresh models list (`POST /models/refresh`) fetches right away. Either way Polza is asked at most once every 30 seconds. A chat request for a model the server doesn't know yet refreshes a stale list first. The browser keeps the last list (`neochat-models-v1`) so the picker fills instantly. `MODEL_CATALOG_LIVE=false` turns all this off.

**Web search and page reading.** The composer's Web search toggle (+ menu) sends `"web_search": "auto" | "on" | "off"`. Models with function calling (`"tool_calling": true` in `configs/models.json`) get two tools, `web_search` and `web_fetch`, and the server runs the loop (`server/websearch.go`): the model asks for tools, the server runs them, sends the results back, and the model goes on. An answer gets at most 8 model calls, 5 searches and 6 pages. Each `web_search` is a small call to `WEB_SEARCH_MODEL` (a catalog model id, default `gpt-6-luna`, reasoning off) with Polza's web plugin (`plugins: [{"id": "web", "engine": "yandex", "max_results": 10, "search_prompt": …}]`). Polza runs the search and returns the results as `url_citation` annotations; the helper model also copies them out as JSON in case a route returns no annotations. Polza's own server tools (`polza:*`) aren't used because they are closed to most accounts ("недоступны для организации"), while the plugin is open to everyone and currently free. `web_fetch` is this server's own: `webfetch` downloads the page and turns HTML into readable text (headings, lists, tables, code and links survive; scripts, navigation, footers and forms don't; no JavaScript). It accepts only http(s) on ports 80/443, with no cookies or credentials, and refuses private, loopback, link-local and other non-public addresses at dial time, so a hostname that resolves to an internal address is refused too. Pages are capped at 5 MB downloaded and 100k characters kept; the model gets 20k characters per call and reads on with `offset`. Pages are kept per chat in `web_pages` (`pagestore`), so reading further or asking about a page again doesn't refetch it. They are deleted with the chat (in the same transaction), expire 24 hours after the fetch, are dropped after an hour if their chat was never stored (stopped or failed answers), and the oldest go first once all pages pass 256 MB. `RunCleanup` does the sweeping hourly. Incognito chats store nothing. In "on" mode tool models are also told to search first. Models without function calling get one plugin search for the user's message before they answer in "on" mode, and in "auto" mode a note telling them to suggest switching to On when asked to search. If Polza refuses the web settings (400/404/422), the answer goes out without them, with the refusal shown as a failed step. If a route can't take a model's reasoning back with its tool calls, the loop carries on with reasoning off. Each step streams as an `activity` SSE event. The UI shows it in place of "Thinking" ("Searching the web", "Reading example.com"), then "Thought for N seconds · 2 searches · 1 page"; opening it lists the searches (query and results) and, separately, the pages read (title, size, start of the text). Steps and the thinking time (`thought_ms`) are stored with each response version. Every web plugin call logs `provider: polza web plugin model=… provider=… engine=… results=…`.

**Staying signed in.** The web UI's sign-in trades the pasted NeoChat key for a session: `POST /auth/session` with `{"api_key": "nc_..."}` sets an `HttpOnly`, `SameSite=Strict` cookie (`neochat_session` over plain HTTP, `__Host-neochat_session` with `Secure` over HTTPS), and the key itself is kept nowhere in the browser. Every endpoint accepts that cookie in place of `Authorization`, provided the request also carries `X-NeoChat-Request: 1` (the CSRF guard: other sites can't add it). `GET /auth/session` says whom the cookie signs in (`{"signed_in": false}` for nobody) and refreshes it; `DELETE /auth/session` is Log out. Sessions live in `web_sessions` (hashes only), last 30 days from their last use, stop working the moment their API key's row is deleted, and are swept hourly by `server.RunCleanup`. API clients keep using `Authorization: Bearer`.

**Quoting part of an answer.** Selecting text inside a finished answer shows an Ask button: above the selection with a mouse, below it on touch screens, where the system's own menu sits above. Ask puts the selection in the composer as a quote card; the next message is sent with `"quote": "…"` (cleaned and capped at 4000 characters server-side, `server/quote.go`). A quote alone isn't a message. The quote is stored with the user message (`conversation_messages.quote`, migration 0011) and reaches the model as a Markdown blockquote before the question, on that turn and in every later turn's history (incognito history and regenerations included). The thread shows it above the user's bubble, two lines until clicked.

**Questionnaires.** Every model with function calling also gets an `ask_user` tool (`server/questionnaire.go`): instead of guessing on an open-ended request, it can put a short questionnaire to the user. The questionnaire has a title, 1-6 questions (each with an optional hint, 0-6 options with optional descriptions, single or multiple choice) and a closing note. Calling it ends the turn. The server tidies the arguments: it caps lengths and counts and drops any "Other"/"Custom" option the model added, because the panel always ends each question with a field for the user's own answer. Malformed arguments go back to the model as a tool error, so it can fix them. The questionnaire is stored with the response version (`questionnaire` in the versions JSON) and sent in the `done` event. The UI opens it as a panel above the composer, or in place of the composer on screens up to 680px, one question at a time: digits pick options, a single choice moves on by itself, multi-select uses Next, and Back, Skip and Finish are always there. Finish sends the answers as the user's next message, one `Q: …` / `A: …` pair per question (`(skipped)` for skipped ones), which the thread shows as a question/answer list. The answer that asked keeps a card that reopens the panel until something is sent after it, also after a reload. In later turns the model sees what it asked as a bracketed note after that answer's text (incognito chats send it back in `incognito_history`). When and how to ask is taught in the shared core of the tone prompts (`prompts/*.md`, "Questions before the answer").

## 1. Start everything

`cmd/server` requires a real Postgres and Redis (see README's "Current task: local dev infrastructure") — the `InMemory*` stand-ins are gone. All three — Postgres, Redis, and the server itself — run in Docker via `docker-compose.yml` at the repo root; there's no separate `go run` step for normal use.

Prereqs, once per machine:
- A swap file if the host is memory-constrained (this repo's dev host is 2 GB RAM — see README point 1 under "Current task").
- Docker + the `docker compose` plugin (`docker compose version` should print something; if it doesn't, install the plugin — see Docker's docs).

```bash
cp .env.example .env    # fill in real POSTGRES_*/REDIS_* values (anything works locally); POLZA_API_KEY is optional
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
| `POLZA_API_KEY` | Optional server-side fallback key, used only for calls that arrive without a user key. Leave empty to require every user to bring their own (Settings → Account). |
| `ROUTER_ENABLED` | Optional. Unset/anything but `true` = router disabled, Manual-only direct mode (see above). |
| `MODEL_CATALOG_LIVE` | Optional. `false` keeps the Manual picker to `configs/models.json`; anything else follows Polza's releases (see above). |
| `MAX_OUTPUT_TOKENS` | Optional, defaults to `16000` (range 1..16000). Hard ceiling on output tokens per generation call. |
| `CLASSIFIER_API_MODEL_ID` | Optional, defaults to `google/gemini-3.5-flash-lite` (the model `prompts/classifier_system_prompt.md` was validated against). Only used with the router enabled, and as the summarizer default. |
| `MODERATION_API_MODEL_ID` | Optional, defaults to `openai/gpt-oss-120b`. Only used with the router enabled. |
| `CLASSIFIER_COST_INPUT_PER_MTOK` / `CLASSIFIER_COST_OUTPUT_PER_MTOK` | Optional, default `0.3` / `2.5` (matches the `CLASSIFIER_API_MODEL_ID` default). Prices the classifier's `cost_log` entries -- override alongside `CLASSIFIER_API_MODEL_ID` if you change it, or logged cost keeps pricing the old model. |
| `MODERATION_COST_INPUT_PER_MTOK` / `MODERATION_COST_OUTPUT_PER_MTOK` | Optional, default `0.03` / `0.17` (matches the `MODERATION_API_MODEL_ID` default). Same caveat as the classifier rates above. |
| `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` | Required. Must match what `docker-compose.yml` started Postgres with. |
| `POSTGRES_HOST` / `POSTGRES_PORT` | Optional, default to `127.0.0.1` / `5432` (docker-compose's published address). |
| `REDIS_PASSWORD` | Required. Must match what `docker-compose.yml` started Redis with. |
| `REDIS_HOST` / `REDIS_PORT` | Optional, default to `127.0.0.1` / `6379`. |
| `IDEMPOTENCY_TTL` | Optional, defaults to `24h` (Go duration syntax). How long a completed `/chat` response stays replayable by `idempotency_key` in Redis. |
| `ADDR` | Optional, defaults to `:8080`. |

**Never commit a real key.** `.env` is gitignored specifically so `POLZA_API_KEY` and the Postgres/Redis credentials never end up in git — don't paste real values into `.env.example` or any other tracked file.

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
export POLZA_KEY=pza_...   # your own Polza AI key
curl -s localhost:8080/chat -X POST \
  -H "Authorization: Bearer $NEOCHAT_API_KEY" \
  -H "X-Provider-Key: $POLZA_KEY" \
  -d '{
  "message": "привет",
  "requested_mode": "manual",
  "manual_model_id": "deepseek-v4.1-flash",
  "reasoning_effort": "low",
  "persona": "Default",
  "instructions": "Answer briefly."
}' | python3 -m json.tool
```

With the router disabled (the default), `requested_mode` must be `manual` with a `manual_model_id` from `configs/models.json` (`deepseek-v4.1-flash` is the cheapest sensible test model); anything else gets `400 Router disabled`. `reasoning_effort` is one of `low|medium|high|xhigh|max` (mapped to Polza's `reasoning.effort`, or `reasoning.type=adaptive` + `effort_level` for Claude Opus 4.7+, where `xhigh` rounds up to `max`).

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
  -H "X-Provider-Key: $POLZA_KEY" \
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
- **`no provider.Client configured for provider "..."`** — the router selected a model from a catalog provider with no wired-up client. Shouldn't happen anymore: `cmd/server/main.go`'s `Generators` map now covers all five catalog providers (`anthropic`, `openai`, `google`, `moonshot`, `deepseek`) (plus `spacexai`) through the same `PolzaClient`. If you see this, either a new catalog entry added a provider tag not yet in that map, or there's a typo between the two.
- **"Polza AI: ... (HTTP 404)"** — `api_model_id` doesn't match a live Polza slug (models get retired). Check `curl https://polza.ai/api/v1/models/<slug>`.
- **"Polza AI rejected your API key"** (401/403) or **"balance is too low"** (402) — the user's own key/balance; shown to them verbatim since it's their account.
- **429** — rate limit on the user's Polza key, not a bug here.
- **401 straight back from `curl localhost:8080/chat`, instant, no server-side classify/moderate log line** — this is neochat's own auth check (step 4 above), not OpenRouter: missing `Authorization` header, or a token `cmd/issuekey` never printed for this database. Re-run `issuekey` and re-export `NEOCHAT_API_KEY` if in doubt -- a stale value from an earlier `.env`/database reset looks identical to a typo.
- **"Add your Polza AI API key"** — the request had no `X-Provider-Key` header (key not saved in Settings → Account) and no `POLZA_API_KEY` fallback.
- **The classifier reply fails to parse as JSON** — `classifier.Classify` already strips a wrapping ` ```json ` fence, but a genuinely different failure (the model refusing, adding prose, etc.) will surface as a clear parse error with the raw reply included — that's real signal about how the model behaves with this prompt through Polza specifically, not necessarily a bug.
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
