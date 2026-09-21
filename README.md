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
- Issuance is a manual CLI for now (`go run ./cmd/issuekey`), not a signup flow — there's no login/registration UI yet.
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
- **Prompt caching vs. routing conflict.** Vendor-side context caching breaks when the model changes mid-session — not fully reconciled with auto-routing yet.
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
- Chat personas: server-side mechanism implemented; prompt text and UI still pending.
- OpenRouter integration: implemented and confirmed live end-to-end, including one fully successful generation through the containerized server, persisted to conversation and cost logs.
- Moderation: Layer 1 implemented and wired in; Layer 2 deliberately deprioritized.
- Streaming: implemented (`POST /chat/stream`), sharing the full pipeline with the non-streaming path.
- Circuit breaker: implemented and wired into failover.
- Conversation storage, tokenizer/context estimation, long-conversation summarization, idempotency, per-request cost logging: all implemented and Postgres/Redis-backed.
- Authentication and per-user rate limiting: implemented — API-key auth, no self-serve signup yet.
- Local dev infrastructure: Docker Compose (Postgres + Redis + the server itself), migrations auto-applied on startup, documented in `docs/running-locally.md`.

What's *not* here, deliberately: a production deployment, a payments integration, a signup flow, or a UI. This is the backend/engineering half of the idea, built far enough to be a real demonstration of the pattern — not a shipped product, for the reasons above.

## License

MIT. See `LICENSE`.
