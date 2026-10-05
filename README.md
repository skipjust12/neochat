# AI aggregator with depth-based modes, not model-based modes

"You choose how deep to think. We choose who does the thinking."

A solo-built, production-grade routing engine for LLM chat: config-driven auto-routing across vendors, spend limits with a guaranteed-margin design, circuit breakers, idempotency, streaming, and Postgres/Redis-backed state throughout. Built as an engineering exercise and portfolio piece — evaluated as a consumer SaaS, and deliberately not launched as one. Reasoning below.

## Table of contents

- [Why this isn't launching as a product](#why-this-isnt-launching-as-a-product)
- [What it is](#what-it-is)
- [Architecture](#architecture)
  - [Routing pipeline](#routing-pipeline)
  - [Config-driven weights](#config-driven-weights)
  - [Classifier output schema](#classifier-output-schema)
  - [Tokenizer / context estimation](#tokenizer--context-estimation)
  - [Moderation](#moderation)
  - [Streaming](#streaming)
  - [Chat personas](#chat-personas-system-prompt-picker)
  - [Spend limits](#spend-limits)
  - [Authentication](#authentication)
- [Pre-launch engineering checklist](#pre-launch-engineering-checklist)
- [Additional architectural risks](#additional-architectural-risks)
- [Infrastructure (MVP)](#infrastructure-mvp)
- [Status](#status)
- [License](#license)

## Why this isn't launching as a product

The original pitch was straightforward: most people don't know which AI model to use for a given task, and juggling separate subscriptions to Claude/ChatGPT/Gemini/etc. is friction nobody wants. Build an aggregator that picks the right model per request, expose that as a simple "how deep should this think" dial instead of a vendor picker, charge a subscription.

Worth writing down honestly why that doesn't hold up, since the architecture below was built to serve it:

**The consumer problem is mostly imagined.** People who actually agonize over "ChatGPT or Claude or Gemini?" are a small, self-selected, terminally-online minority — visible on tech Twitter precisely because that's a filter bubble of people who think about this stuff. The much larger reality: for most users, "AI" is already a synonym for one brand (ChatGPT), the same way "Google" is a synonym for search. There's no felt choice to simplify, because there's no felt choice happening in the first place.

**Where the problem is real, it's not a consumer problem.** Frontier models have converged enough on general quality that picking between top-tier models for an everyday question is mostly a style/personality choice, not a capability choice — a consumer won't reliably perceive the difference. The gap that *is* large and measurable shows up in agentic/coding workloads (terminal use, long tool chains, structured code gen), and the people who care about that are developers, not the "which model?" confused end user this product was originally aimed at.

**Precedent already answered this.** Poe has run this exact positioning — cross-vendor aggregation, single subscription, intelligent routing — since 2023, backed by Quora's capital, and stayed a niche product for AI enthusiasts. It never went mass-market. If three years and real funding couldn't do it, a solo project isn't going to out-execute that on this axis.

**The market that *does* exist for this is B2B infra, and it's already won.** Model routing has real, large, fast-growing demand — OpenRouter raised $113M at a $1.3B valuation in May 2026 and was reportedly acquired by Stripe for $7B+ a few months later; Ramp shipped its own router; Cursor, Meta, and others are building theirs. But the demand driving all of that is enterprise/developer cost control on API spend (agents burning unpredictable token budgets), not consumer confusion in a chat UI. That space is real — and it's not a space a nameless solo project enters against a $7B incumbent with a three-year head start and enterprise trust already built.

**The economics don't favor a reseller either.** Vertically integrated vendors (OpenAI, Anthropic, etc.) can price their own cheap-tier models at or below what a reseller pays at retail API rates, because they're pricing near their own inference cost, not a markup on someone else's. A margin business built on "cheap access to someone else's models" is structurally squeezed from the start.

Net: the architecture is sound and the engineering is real, but it's solving a problem that doesn't exist at the scale needed to be a business, in a market segment that's already occupied where the problem *is* real. It stays a portfolio project — MIT-licensed, public, meant to demonstrate the engineering rather than to be operated as a service.

## What it is

A ChatGPT-equivalent chat app (not just an aggregator) with 5 modes, chosen on an everyday "how deep should this think" axis instead of a vendor/model axis:

| Mode | Behavior |
|---|---|
| Auto | the system decides what's needed |
| Instant | fast response (e.g. a cheap flash-tier model) |
| Thinking | longer/deeper reasoning (e.g. a mid-tier reasoning model) |
| Max | maximum quality, most expensive processing (top-tier models) |
| Manual | user picks the model directly, for full control |

Plus voice, image, and a full chat UI feature set. Cheap models are used for routing wherever possible without a quality hit, and the system prompt is designed so a model switch mid-conversation is rarely noticeable.

## Architecture

### Routing pipeline

```
Request
  ↓
Classifier (cheap model)
  ↓
JSON with request analysis
  ↓
Go router
  ↓
Model selection
  ↓
Response to user
```

Routing principles:
- Don't pick the cheapest model.
- Pick the cheapest model that preserves quality.
- When in doubt, escalate to a stronger model.

What the classifier analyzes: task type, language, code/text/image, reasoning depth, creativity, whether web search is needed, expected response length, query complexity.

### Config-driven weights

The router code is an engine, not where the logic lives — routing behavior is entirely config-driven, versioned, and swappable without a deploy.

```
/configs/
  weights_v1.json
  weights_v2.json
  weights_experimental.json

active_config.json:
{
  "default": "weights_v1",
  "buckets": {
    "0-4": "weights_v2",    // 5% of users on the new version
    "5-99": "weights_v1"    // everyone else on the old one
  }
}
```

Runtime behavior:
- On startup, the router loads `active_config.json` to learn which weights file is active for which user bucket.
- Per request: `bucket = hash(user_id) % 100` → look up `active_config.json` → load the corresponding `weights_*.json` → apply scoring.
- Weights are kept in memory with a TTL, or invalidated on event (file changed / Redis key updated) — not read from disk per request, but no restart needed to pick up changes either.

Requirements for "just change the config file and it works":
- Weight files are data, not code — parsed JSON/YAML, not Go structs compiled into the binary.
- A config watcher — polling every N seconds, or pub/sub (Redis PUBLISH/SUBSCRIBE, or fsnotify on the file) — the router re-reads `active_config.json` without a process restart.
- Atomic swap of the active config — change `active_config.json` (or a Redis key), not the weight files themselves — this is the single point of control.
- Config validation before activation — mandatory, otherwise a JSON typo in prod breaks routing for everyone at once. Simple schema check on load; on failure, stay on the last known-good version instead of crashing.

Result: rolling back or switching is a one-line change (`"default": "weights_v1"` → `"weights_v3"`), picked up by the router within seconds, with zero dropped connections, no code deploy, no process restart.

### Classifier output schema

```json
{
  "schema_version": "1.1",
  "task_category": "software_engineering",
  "task_intent": "generate",
  "language": "en",
  "modality_input": ["text", "code"],
  "modality_output_expected": ["text", "code"],
  "reasoning_depth": "moderate",
  "creativity_level": "low",
  "required_tools": ["code_execution"],
  "expected_output_length": "medium",
  "estimated_output_tokens": 800,
  "output_format": "markdown",
  "complexity_score": 0.6,
  "context_dependency": "light",
  "confidence": 0.82,
  "content_flags": [],
  "safety_risk_score": 0.0
}
```

Design principle: the classifier returns raw task features, never a final model decision. The feature → model mapping lives entirely in `weights_*.json` on the router side — this keeps model reweighting a config change, not a classifier prompt rewrite.

- `task_category` is the capability/domain axis: general, writing, frontend, software_engineering, data_analysis, math, research, reasoning, knowledge, translation, vision, or image_generation.
- `task_intent` is the operation axis: answer, generate, edit, debug, review, explain, summarize, compare, plan, extract, classify, or transform.
- The router combines the two against each model's `task_category_scores` and `task_intent_scores`. Within the selected mode it retains models close enough to the best task fit (`task_profile.minimum_score` and `task_profile.max_quality_gap`), then chooses the cheapest survivor. Unknown classifier values fall back to general/answer and are recorded in `RouteResult.Reason`.
- `expected_output_length` is a bucketed enum, not a raw token estimate — small models are unreliable at precise token counts.
- `confidence` measures certainty in the classifier's labels, not task difficulty. Low confidence is recorded in the route reason but does not change the tier by default. Operators can restore one-tier escalation for auto requests with `confidence_escalation_enabled`; explicit instant/thinking/max choices are never overridden by classifier uncertainty.
- `context_dependency` feeds the prompt-caching-vs-routing tradeoff (see below): `heavy` should penalize mid-session model switches more aggressively.
- Cost estimation (input token count) is not a classifier field — it must be computed deterministically by a tokenizer before any request goes out, as a hard defense against cost-based abuse.
- `schema_version` is required from day one, since this schema will evolve the same way the routing weights do.

### Tokenizer / context estimation

`tokenizer.EstimateMessages` (`tokenizer/`) satisfies the "computed deterministically by a tokenizer before any request goes out" requirement above — it replaces `chatRequest.EstimatedContextTokens` (a client-supplied, unverifiable number, still accepted on the wire for backward compatibility but no longer read by `server.prepare`) with a server-side estimate computed from the real system prompt + full stored conversation history + new user message.

- **Not a real BPE tokenizer.** The catalog spans multiple vendors (Anthropic, OpenAI, Google, Moonshot, DeepSeek), each with its own vocabulary, so no single exact tokenizer would even be correct for every model in it. Instead it's a heuristic: `max(chars/4, words × 1)` per message plus a small fixed per-message overhead, deliberately biased to overestimate rather than undercount, since this value feeds a hard filter and a cost pre-estimate.
- **Closes two gaps at once:** (1) *Context window mismatch* — the router's context-window hard filter and cost pre-estimate (`router/cost.go`) no longer silently drift from reality as a conversation grows; `estimatedContextTokens` is recomputed from the actual message list on every turn. (2) *Cost-based abuse* — a client can no longer understate `estimated_context_tokens` to slip a request under a spend cap, since the number that reaches `Route` is derived from the request body itself.
- `server.prepare` folds the older part of a conversation into a rolling summary once its estimated size crosses `Server.SummaryTriggerTokens` (`summarizer/`), so the hard filter trips on real data that stops growing without bound.

### Moderation

Two-layer design, only one layer implemented — never blocking on UX latency.

**Layer 1 — input (before showing the response to the user) — implemented** (`moderation/`, wired into `server.handle`)

- Runs concurrently with classification (`sync.WaitGroup`, two goroutines) — both are cheap-model calls over the same raw request text, so there's no reason to pay their latency twice.
- A cheap model checks the request text against `prompts/moderation_system_prompt.md` and returns `{flagged, categories, reason}` — same JSON-over-a-cheap-model pattern as the classifier.
- `flagged` → generation is skipped entirely and the user sees a fixed ToS violation message (`chatResponse.Blocked`); the flagging model/reason is logged server-side, not exposed to the client.
- Block events are persisted, not just logged: every flag calls `moderation.BlockLog.Record` (user_id, categories, reason, timestamp). Postgres-backed (`moderation.PostgresBlockLog`); `moderation.InMemoryBlockLog` still exists for tests.
- Fails closed: a moderation call erroring (as opposed to flagging) fails the whole request rather than letting an unmoderated message through.

**Layer 2 — output (during streaming) — deliberately deprioritized, not currently planned**

Originally specced as scanning the output stream mid-generation as a backstop against jailbreaks. Dropped for now, not just left undone:
- The catalog only routes through models with their own vendor-side safety tuning — the marginal case Layer 2 catches is narrow (a jailbreak that both slipped past Layer 1 *and* got a frontier model to comply anyway).
- Real added cost: a second moderation-model call per request, this time against streamed output.
- No usage data yet to size thresholds against. Revisit if Layer 1's block logs or a real incident show jailbreaks getting through — that's the trigger, not a calendar date.
- Cheapest available mitigation without building Layer 2: retroactively re-running Layer 1's check against persisted conversation messages for audit purposes — not implemented, just cheaper to add later if it matters.

### Streaming

`POST /chat/stream` delivers a reply incrementally over Server-Sent Events, alongside (not replacing) `POST /chat` — same request shape, same pipeline (`server.prepare` → `route+generate` → `server.finalize`).

- `provider.StreamingClient` is a second, optional interface (`GenerateStream(ctx, apiModelID, messages) (<-chan StreamChunk, error)`) alongside `Client.Generate`. `provider.OpenRouterClient`, `provider.FakeClient`, and `provider.CircuitBreakerClient` all implement it, so streaming works automatically across the whole provider set.
- `StreamChunk` carries either an incremental `Delta`, or a terminal chunk: `Done` + `Final` on success, or `Err` on failure.
- `server.handleStream` shares `prepare`/`routeAndCall`/`finalize` with `handle` — the circuit-breaker failover loop is identical code for both paths. Event types over SSE: `meta` (model picked, fires again on failover), `delta`, `done`, `blocked`, `error`.

### Chat personas (system prompt picker)

Five named personas instead of one fixed system prompt: Default, Expert, Friendly, Cynical, Direct.

- `server.SystemPromptNames` is the single source of truth for the five names.
- One prompt file per persona under `prompts/`. All five currently exist as empty placeholders — plumbing shipped ahead of the actual prompt text, on purpose.
- `server.LoadSystemPrompts(dir)` loads all five at startup. Unlike the classifier/moderation prompts (fatal if missing), a missing persona file here is just skipped and logged — selecting it sends no system message at all, which is what lets the plumbing ship before the text exists.
- `chatRequest.persona` picks one by exact name; empty defaults to `"Default"`; an unrecognized name is rejected with 400 before any classify/moderate/route/generate work happens.
- No UI exists yet to expose this picker — server-side half only.

### Spend limits

Full design and numbers: `docs/unit-economics.md` section 6. Implemented in `limits/`.

- Two independent pools, not one budget. Instant is near-free (~$0.0014/request) and never fully blocked — even a locked-out user keeps a working chat. Thinking+Max is the actual constrained resource and the only one hard-capped.
- One 30-day rolling cap per plan (not a fixed calendar month, to avoid the boundary-doubling exploit of burning the cap on day 30 and again on day 1). Set below the plan price on purpose (e.g. Pro cap $10 against a $19 price), so the worst case still guarantees real margin. A separate, smaller Instant cap exists purely as an anti-bot ceiling.
- `limits.SpendStore` is the interface seam between accounting logic and where spend actually lives. Redis-backed (`limits.RedisSpendStore`) — one key per (userID, pool, hourly bucket), TTL-expired.
- `router.Route` takes a `thinkingMaxLocked` bool. When true, it forces Thinking/Max down to Instant, and rejects (rather than silently substitutes) a thinking/max-tier `manual_model_id` — manual mode is explicitly the loophole that would otherwise let a locked-out user keep hitting an expensive model directly.
- A three-layer version (5h/7d/30d rolling windows) was designed and deliberately dropped for v1: typical usage sits well below the monthly cap, so sub-windows would mostly protect against what the monthly cap already catches, sized against guesses rather than real usage data.

### Authentication

Implemented (`auth/`, wired into `server.Mux()`). Every `user_id`/`plan_id` that spend limits, routing tier, conversation history, and the moderation log depend on now comes from an authenticated caller instead of the request body — closing the gap that let a client get unlimited free access by inventing a new `user_id` per request.

- `Authorization: Bearer <api-key>` on every chat call, checked by middleware between the per-IP rate limiter and the handler (so a garbage token is rejected cheaply before it drives a database lookup). `chatRequest.UserID`/`PlanID` are `json:"-"` — client-supplied values are silently ignored.
- `auth.Authenticator` is the one method (`Authenticate(ctx, token) (Identity, error)`) `server.Server` actually depends on, so a future real auth system is a new implementation of this interface, not a pipeline rewrite.
- `auth.PostgresStore`, backed by an `api_keys` table storing only a token's SHA-256 hash — a leaked table dump doesn't hand out working credentials.
- Issuance is a manual CLI for now (`go run ./cmd/issuekey`), not a signup flow — there's no registration UI yet.
- The web UI doesn't keep the key: signing in trades it for a browser session (`POST /auth/session`, `auth.SessionStore`, `web_sessions` table, hashes only) carried in an `HttpOnly`, `SameSite=Strict` cookie (`__Host-` and `Secure` over HTTPS). Every endpoint accepts that cookie in place of the header, but only together with `X-NeoChat-Request: 1`, which other sites can't send. A session lasts 30 days from its last use, survives closing the tab or browser, ends on Log out (`DELETE /auth/session`) and dies with its API key.
- Rate limiting is two-keyed: per-IP (catches callers with no valid key) and per-user (catches one key driving traffic from many source addresses). Both fail open if Redis is unreachable — a degraded throttle is a bounded loss, a self-inflicted outage isn't.
- Not done: key revocation/expiry (a leaked key is invalidated by hand today).

## Pre-launch engineering checklist

Things worth baking in early because retrofitting them on a live system later is expensive or breaks compatibility. Status of each, as implemented:

1. **Model catalog as data, not code** — done. Models, weights, prices, provider params live in the config system; adding a model is a config edit, not a deploy.
2. **Provider abstraction layer** — done. `ModelProvider`-style interface behind which OpenRouter or direct vendor APIs can sit, so a hybrid-sourcing migration isn't a rewrite.
3. **Request-level idempotency and cancellation** — done, and verified with tests, not just assumed. `r.Context()` propagates from the HTTP handler through classify/moderate/generate to the real outbound OpenRouter call; three tests pin this down against real cancellation, not an assumption. Idempotency (`idempotency/`, Redis-backed): a client retry with the same `idempotency_key` replays the original response instead of triggering a second charge; a concurrent/retried request arriving mid-flight is rejected outright rather than raced.
4. **Circuit breaker / per-model health tracking** — done, including failover. `provider.CircuitBreakerClient` fast-fails a model after consecutive failures for a cooldown period; `router.Route` takes an `excludedModelIDs` set so a failed model gets excluded and the request retried on a healthy alternative (bounded attempts). Manual mode is the one exception — no substitute exists for a model requested by name.
5. **Conversation storage schema built for migration** — done (`conversation/`, Postgres-backed). Every stored assistant turn records which model answered it. Known, deliberate limitation: classification/moderation still only see the newest message, not full history.
6. **Stateless app layer** — done. Every store (spend limits, idempotency, conversation, moderation blocks, cost log) is Redis/Postgres-backed; a second server instance against the same database would already share state correctly.
7. **Per-request cost accounting** — done. Every generation call logs a `router.CostLogEntry` (user_id, model, tokens, cost_usd, timestamp) to Postgres. Classifier and moderation calls are logged too, sharing one `RequestID` per `/chat` call so all three billed calls reconcile as a group.
8. **Secrets/API keys via env/vault from commit one** — done (gitignored `.env`, `.env.example` documenting every variable).

## Additional architectural risks

Known, not all resolved:

- **OpenRouter as a single point of failure.** No direct vendor contracts yet as an emergency fallback if the whole platform goes down.
- **Prompt caching vs. routing conflict.** Caching works per model (Claude through explicit cache points, the others on their own), and switching models mid-chat starts a fresh cache — not reconciled with auto-routing yet, which could switch every turn.
- **Context window mismatch** — addressed via the tokenizer + summarization work above.
- **Prompt injection against the system prompt.** Users can try to extract it and figure out which model is actually answering — undermines the "don't think about models" positioning if it becomes common, though not a security issue per se.
- **Cost-based abuse** — addressed: output token caps, request body size limits, per-IP/per-user rate limiting, and authenticated `user_id` (no more resetting spend history by fabricating a new one).
- **Onboarding and cognitive load.** Five modes can overwhelm a new user even though the point of the product is removing the burden of choice — needs a sane default explained through UX, not text.
- **`/health` doesn't check real dependencies.** Always returns 200 regardless of whether Postgres/Redis/OpenRouter are actually reachable.
- **Classifier's safety signal is parsed but never used.** `ContentFlags`/`SafetyRiskScore` are decoded from every classify call but nothing reads them yet.
- **No structured logging.** Every log line is free-form `log.Printf` — no levels, no queryable fields.
- **No data retention or deletion story.** Full message text stored indefinitely, no TTL, no export/delete path, no privacy policy yet.
- **`plan_id` has no connection to real payment.** Today it's whatever an operator typed into the CLI issuer — expected at this stage, not a gap in the auth work itself.


## Status

- Go router: implemented — feature-based scoring, hard filters, manual-mode passthrough, deterministic cost-tie breaking, spend-lock override, circuit-breaker-aware failover.
- Spend limits: implemented, Redis-backed, 30-day rolling cap per plan.
- Classifier: prompt written and validated against live model calls, wired into every request.
- Chat personas: implemented end to end (tone picker in Settings → server persona prompts in `prompts/<Name>.md`); prompt text still to be written.
- Model provider: Polza AI (OpenAI-compatible, `provider.PolzaClient`) replaced OpenRouter. Users bring their own Polza key (Settings → Account), sent per request and never stored server-side.
- File attachments: images (PNG/JPEG/GIF/WebP), PDF/DOCX and text/code files. Uploaded via `POST /files` into Postgres, sent to the model as native image/file parts (text files inlined, so any model reads them), re-sent on later turns, deleted with the chat. Models that can't read a file type say so before anything is sent.
- Image generation: picking Nano Banana Pro or Nano Banana 2 lite in Manual makes a picture through Polza's Media API on a separate Image API key (Settings → Account). A dot field with progress phrases shows while it's made. Follow-ups edit the last picture, and pictures are kept as the chat's files and deleted with it (`server/image.go`, `imagegen/`).
- Artifacts: a complete HTML page a model writes (```html with a whole document; the core prompt teaches when to make one) appears as a card and opens full screen with a Close button. It runs in an iframe sandboxed without same-origin, under its own CSP (`GET /artifact-frame`).
- LaTeX formulas ($…$, $$…$$, \(…\), \[…\]) rendered with a vendored KaTeX.
- Search through every chat's titles and messages (`GET /conversations/search`); the sidebar lists all chats, no longer the latest 100. Projects can be deleted, and their chats move back to Recent.
- Settings → Usage shows the Polza balance behind the user's key (`GET /account/balance`).
- Replies survive a dropped connection: generation runs detached from the request, the client rejoins the numbered stream (`GET /chat/stream/{id}?after=N`) up to five times with "Streaming interrupted, retrying (n/5)", a reloaded page picks the reply back up, and Stop is explicit (`POST /chat/stop`).
- The model's thinking streams along with the answer and opens on a tap on what it's doing ("Pondering", "Thought for N seconds"); it's kept with the answer.
- Replies are live on every device: `GET /events` announces them, an open chat follows the reply as it's written, the sidebar marks chats with a reply in progress, and Stop works from any device.
- In Manual the composer shows the chosen model with its vendor's logo instead of "Manual".
- "+" → Sketch: a small drawing board (pen, eraser, rectangles, ellipses, lines, arrows and text that can be moved and resized, six colors, undo). Text is typed in place; a corner scales its letters. Confirm attaches the drawing to the message as a picture; it goes out with the message.
- Unread replies: a chat whose reply finished where nobody was looking (another device, a page in the background) shows a dot in the sidebar until it's opened on any device (`POST /conversations/{id}/read`, `read` on `GET /events`, column `conversation_metadata.read_at`).
- Pictures in a chat (sent and generated) open full screen: zoom with a pinch, double tap or the wheel, swipe or arrow between them, swipe down to close, Download.
- On phones a new chat starts with the message box at the bottom, where it stays once the chat starts, and without the suggestion list; desktops keep the centered box.
- Low balance alerts: thresholds in Settings → Usage (300 ₽ and 100 ₽ by default, synced with the other settings). Below the first a note above the message box reads "Low balance approaching", below the second it asks to top up; sending is never blocked.
- The Manual model list follows Polza: new releases join it on their own (Claude Haiku 5.5 takes Claude Haiku 4.5's place, which moves to Legacy), checked when the app opens (at most every 10 minutes) or from Settings → General → Refresh models list (`GET /models`, `POST /models/refresh`, package `modelcatalog`).
- Settings sync: tone, instructions, language, theme, the default model and web search follow the user between devices (`GET/PUT /account/settings`, table `user_settings`). API keys never leave the browser.
- "+" → Compact: after a yes/no confirmation, GPT-6 Luna (on the user's key) folds everything but the last exchange into the chat's summary (`POST /conversations/{id}/compact`); later turns send the summary instead, and the thread marks where it happened.
- Incognito keeps the home greeting, reading "Incognito mode"; the thinking label rotates every 10s through Fabrication, Hallucinating, Thinking, Overthinking, Pondering, Calculating, Predicting, Cooking, Synthesizing, Decoding, Guessing and Brewing.
- Prompt caching: Claude requests mark three cache points (the end of the system messages, and the last two user messages; `provider/cache.go`), so a long chat re-reads its history from Polza's cache at a tenth of the price; OpenAI, DeepSeek, Gemini and Grok cache on their own. Spend is settled on what Polza says it charged (`usage.cost_rub`, cache discounts included), with cache reads and writes in the server log.
- Streamed answers fade in as they arrive (240 ms per piece, off with reduced motion), at the stream's own pace.
- A long paste (3000+ characters or 50+ lines) becomes a "Pasted text" attachment instead of filling the message box; a click previews it, with "Paste as text" to put it back. In the thread, text files open in the same preview (Copy, Download), and long messages fold to their first lines behind "Show more".
- Code blocks show their language and a Copy button and are highlighted by a vendored highlight.js.
- Quoting: select words in an answer and press Ask, and the selection lands in the composer as a quote card. The next message goes out with it (`"quote"`), the model reads it as a `>` blockquote in front of the question, and the thread shows it above the user's message, history and regenerations included.
- Questionnaires: on open-ended requests a model can ask a few questions first (`ask_user`). They open as a panel above the composer (in its place on phones), with single and multiple choice and a field for the user's own answer on every question, and go back to the model as a `Q: … / A: …` message.
- Web search and page reading, driven by the Web search toggle: models call `web_search` (Polza's web plugin, 10 results) and `web_fetch` (pages fetched by this server, SSRF-safe, kept per chat for a day and deleted with the chat). The thinking block shows "Searching the web" / "Reading example.com" live and keeps the queries, results and pages read in chat history.
- Router: switched off for now (`ROUTER_ENABLED` unset). Only Manual mode works — the chosen model is called directly with the selected reasoning effort, tone and custom instructions; other modes answer "Router disabled". The routing code stays intact for when it comes back.
- Moderation: Layer 1 implemented and wired in; Layer 2 deliberately deprioritized.
- Streaming: implemented (`POST /chat/stream`), sharing the full pipeline with the non-streaming path.
- Circuit breaker: implemented and wired into failover.
- Conversation storage, tokenizer/context estimation, long-conversation summarization, idempotency, per-request cost logging: all implemented and Postgres/Redis-backed.
- Authentication and per-user rate limiting: implemented — API-key auth, browser sessions that keep the web UI signed in for 30 days, no self-serve signup yet.
- Local dev infrastructure: Docker Compose (Postgres + Redis + the server itself), migrations auto-applied on startup, documented in `docs/running-locally.md`.

What's *not* here, deliberately: a production deployment, a payments integration, a signup flow, or a UI. This is the backend/engineering half of the idea, built far enough to be a real demonstration of the pattern — not a shipped product, for the reasons above.

## License

MIT. See `LICENSE`.
