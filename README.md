# neochat
# NeoChat (working title)

AI aggregator with depth-based modes, not model-based modes.

> **"You choose how deep to think. We choose who does the thinking."**

## Table of contents

- [Positioning](#positioning)
- [What it is](#what-it-is)
- [Competitive landscape](#competitive-landscape)
- [Key decisions](#key-decisions)
- [Open questions](#open-questions)
- [Architecture](#architecture)
  - [Routing pipeline](#routing-pipeline)
  - [Config-driven weights](#config-driven-weights)
  - [Classifier output schema](#classifier-output-schema)
  - [Moderation](#moderation)
  - [Spend limits](#spend-limits)
- [Pre-launch engineering checklist](#pre-launch-engineering-checklist)
- [Additional architectural risks](#additional-architectural-risks)
- [Infrastructure (MVP)](#infrastructure-mvp)
- [Status](#status)
- [Next steps](#next-steps)

---

## Positioning

The user operates on a familiar, everyday axis — quick / normal / serious thinking — not a technical one (which model, which vendor). All the complexity of picking the right model for a given task is hidden internal product work, not the user's job.

This differentiates the product from:

- **ChatGPT** — the choice is fully hidden, the user controls nothing, always a single vendor.
- **Poe** — the choice exists, but it's a choice of *models/bots*, which requires AI literacy and enthusiasm for the model landscape.
- **T3.chat** — a developer tool built for speed and flexibility, not for "depth as the core metaphor."

Target user: not an "AI enthusiast," not "AI for dummies," but a **pragmatist** — someone who understands the difference between "dash something off" and "actually think about this," but has no interest in learning about models or vendors.

## What it is

A ChatGPT-equivalent (not just an aggregator) with 5 modes:

| Mode | Behavior |
|---|---|
| **Auto** | the system decides what's needed |
| **Instant** | fast response (e.g. Gemini 3.6 Flash) |
| **Thinking** | longer/deeper reasoning (e.g. Claude Sonnet 5) |
| **Max** | maximum quality, most expensive processing (Opus 5, Fable 5, Sol 5.6, etc.) |
| **Manual** | user picks the model directly, for full control |

Plus: voice, image, a polished UI — full ChatGPT-tier feature set (minus Codex).

Cheap models are used for routing wherever possible without a quality hit. The system prompt is designed so the user almost never notices a model switch.

**Pricing:** Pro — $19 · Pro+ — $100 · Max — $200

## Competitive landscape

- **Poe (Quora)** — closest structural analog: cross-vendor, single subscription, intelligent routing exists. After 3+ years and Quora's capital behind it, it stayed a niche product for "AI enthusiasts," never went mass-market. Differentiator: simplicity (4 legible modes instead of 200+ bots) and an explicit "depth" metaphor instead of a "model" metaphor.
- **T3.chat** — solo/small team, a successful small business, not a unicorn. Realistic scale reference point.
- **ChatGPT** — already partially implements a similar metaphor (Instant/Medium/High/Extra High), but only within a single vendor (GPT). This validates the pattern works, but confirms the cross-vendor layer is exactly what it's missing.

**Market takeaway:** the "aggregator with good UX" niche produces a stable but not massive business (T3/Poe are the proof). Realistic goal: not a unicorn — a profitable product with tens of thousands of paying users.

## Key decisions

| Question | Decision |
|---|---|
| Audience | Consumer, but narrowed: pragmatists, not "everyone" and not niche enthusiasts |
| Monetization | Hybrid: subscription + limits, credits for top-tier models |
| Launch priority | Auto-routing accuracy, UX, ChatGPT-tier feature parity |
| Model access | OpenRouter at launch |
| Engineering | Solo, tech lead |
| Funding | Self-funded; willing to run at a loss early on, not a blocker without outside investment |
| Geography | Global, excluding RU, BY |
| Expensive-mode limits | Tiered plans with hard limits on Thinking/Max |
| Platform | Web first, native apps later |
| Initial acquisition | Paid ads (SMM/targeting), X, etc. |
| Team | None yet, hiring on an as-needed basis |

## Open questions

1. **Training data for the auto-routing classifier.** This is bottleneck #1 — without data (query → which model gave the best answer) there's nothing to build a classifier on. Options: a proprietary labeled dataset, open benchmarks (lmsys, MMLU), user feedback (thumbs up/down as auto-labeling), off-the-shelf solutions (Not Diamond, RouteLLM) as a stopgap.
2. ~~Unit economics are not modeled yet.~~ Modeled — see [`docs/unit-economics.md`](docs/unit-economics.md): typical margin ~81–86% (recomputed 2026-08-11 against current `models.json` prices), plus a guaranteed-margin plan for the Thinking+Max spend cap (`limits/`, see below).

## Overall assessment

Viable as a niche/prosumer product with a clear, recently-found positioning that neither Poe, T3, nor ChatGPT occupies in this exact form. The main risks are execution, not the idea: a classifier with no training data, unmodeled unit economics, and potential positioning drift between "for everyone" and "for people who get it."

## Next steps

1. Lock the positioning as the basis for the landing page/pitch.
2. Narrow the MVP to a single hypothesis: 3-4 modes + text chat, no voice/image in v1.
3. Decide the initial routing approach (heuristics → classifier on proprietary data).
4. Model unit economics on real OpenRouter pricing with a hard cap per plan.

---

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

**Routing principles:**

- Don't pick the cheapest model.
- Pick the cheapest model that preserves quality.
- When in doubt, escalate to a stronger model.

**What the classifier analyzes:**

- Task type
- Language
- Code / text / image
- Reasoning depth
- Creativity
- Whether web search is needed
- Expected response length
- Query complexity

### Config-driven weights

Config-driven architecture: the router code is an engine, not where the logic lives.

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

**Runtime behavior:**

1. On startup, the router loads `active_config.json` to learn which weights file is active for which user bucket.
2. Per request: `bucket = hash(user_id) % 100` → look up `active_config.json` → load the corresponding `weights_*.json` → apply scoring.
3. Weights are kept in memory with a TTL, or invalidated on event (file changed / Redis key updated) — not read from disk per request, but no restart needed to pick up changes either.

**Requirements for "just change the config file and it works":**

- Weight files are data, not code — parsed JSON/YAML, not Go structs compiled into the binary.
- A config watcher — polling every N seconds, or pub/sub (Redis PUBLISH/SUBSCRIBE, or `fsnotify` on the file) — the router re-reads `active_config.json` without a process restart.
- Atomic swap of the active config — change `active_config.json` (or a Redis key), not the weight files themselves — this is the single point of control.
- Config validation before activation — mandatory, otherwise a JSON typo in prod breaks routing for everyone at once. Simple schema check on load; on failure, stay on the last known-good version instead of crashing.

**Result:** rolling back or switching is a one-line change (`"default": "weights_v1"` → `"weights_v3"`), picked up by the router within seconds, with zero dropped connections, no code deploy, no process restart.

### Classifier output schema

```json
{
  "schema_version": "1.0",
  "task_type": "code_generation",
  "language": "en",
  "modality_input": ["text", "code"],
  "modality_output_expected": ["text", "code"],
  "reasoning_depth": "moderate",
  "creativity_level": "low",
  "needs_web_search": false,
  "needs_code_execution": false,
  "expected_output_length": "medium",
  "complexity_score": 0.6,
  "context_dependency": "light",
  "confidence": 0.82
}
```

Design principle: the classifier returns **raw task features**, never a final model decision. The feature → model mapping lives entirely in `weights_*.json` on the router side — this keeps model reweighting a config change, not a classifier prompt rewrite.

- `expected_output_length` is a bucketed enum, not a raw token estimate — small models are unreliable at precise token counts.
- `confidence` drives the escalation rule directly: below a threshold, the router bumps the mode tier up (`instant → thinking`, `thinking → max`) regardless of what the other features say.
- `context_dependency` feeds the prompt-caching-vs-routing tradeoff (see below): `heavy` should penalize mid-session model switches more aggressively.
- Cost estimation (input token count) is **not** a classifier field — it must be computed deterministically by a tokenizer before any request goes out, as a hard defense against cost-based abuse.
- `schema_version` is required from day one, since this schema will evolve the same way the routing weights do.

### Tokenizer / context estimation

`tokenizer.EstimateMessages` (`tokenizer/`) is what actually satisfies the "computed deterministically by a tokenizer before any request goes out" requirement above — it replaces `chatRequest.EstimatedContextTokens` (a client-supplied, unverifiable number, still accepted on the wire for backward compatibility but no longer read by `server.prepare`) with a server-side estimate computed from the real system prompt + full stored conversation history + new user message, i.e. exactly the `[]provider.Message` slice about to be sent to `Generate`.

- **Not a real BPE tokenizer.** The catalog spans multiple vendors (Anthropic, OpenAI, Google, Moonshot, DeepSeek), each with its own vocabulary, so no single exact tokenizer would even be correct for every model in it — pulling in a real one (e.g. tiktoken) would only be exactly right for the subset of models that happen to share its vocabulary. Instead it's a heuristic: `max(chars/4, words × 1)` per message plus a small fixed per-message overhead, deliberately biased to overestimate rather than undercount, since this value feeds a hard filter and a cost pre-estimate — undercounting either is worse than a mild overestimate.
- **Closes two gaps at once.** (1) "Context window mismatch" (see "Additional architectural risks" below): the router's context-window hard filter and cost pre-estimate (`router/cost.go`) no longer silently drift from reality as a conversation grows a real history — `prepared.estimatedContextTokens` is recomputed from the actual message list on every turn, not stuck at whatever number (if any) the client declared once. (2) "Cost-based abuse": a client can no longer understate `estimated_context_tokens` to slip a request under a spend cap, since the number that actually reaches `Route` is derived from the request body itself, not taken on faith.
- **Now addressed:** `server.prepare` folds the older part of a conversation into a rolling summary once its estimated size crosses `Server.SummaryTriggerTokens` (`summarizer/`, see "Context window mismatch" below) — the hard filter now trips on real data, and that real data stops growing without bound.

### Moderation

Two-layer, parallel moderation — never blocking on UX latency.

**Layer 1 — input (before showing the response to the user) — implemented (`moderation/`, wired into `server.handle`)**

- Runs concurrently with classification (`sync.WaitGroup`, two goroutines) — both are cheap-model calls over the same raw request text, so there's no reason to pay their latency twice.
- A cheap model checks the request text against `prompts/moderation_system_prompt.md` and returns `{flagged, categories, reason}` — same JSON-over-a-cheap-model pattern as the classifier (`classifier/`), including the same fenced-reply defense.
- `flagged` → generation is skipped entirely (never even attempted) and the user sees nothing but a fixed ToS violation message (`chatResponse.Blocked`); the flagging model/reason is logged server-side, not exposed to the client.
- **Block events are persisted, not just logged.** Every flag calls `moderation.BlockLog.Record` (`user_id`, `categories`, `reason`, `timestamp`) in addition to the `log.Printf` line — needed to eventually answer "how often, which categories, is this user repeatedly hitting the filter" instead of only ever seeing one flag in isolation in process stdout. **Postgres-backed as of 2026-08-12** (`moderation.PostgresBlockLog`, wired into `cmd/server`) — one row per flagged request in `moderation_block_log`. `moderation.InMemoryBlockLog` (in-process slice behind a mutex) still exists and backs the unit tests that don't need a real Postgres. A failure to *record* a block never un-blocks it — the block itself already happened by the time `Record` is even called.
- **Deviation from the original design, and why:** the spec above (start generation concurrently into a server-side buffer, discard on flag) is a streaming-pipeline optimization. `POST /chat/stream` now exists (see below), but `server.prepare`/`handle`/`handleStream` still wait on both classify and moderate results before routing or generating either way — starting generation early only pays off once there's a Layer 2-style output check to make discarding-on-flag meaningful, and Layer 2 itself is deliberately deprioritized (see below); until/unless that changes, starting generation early would just waste money for the same latency.
- **Fails closed:** a moderation call erroring (as opposed to flagging) fails the whole request rather than letting an unmoderated message through — an outage of the moderation model is currently an outage of chat. Revisit once the circuit-breaker item (pre-launch checklist) makes a softer fallback safe.

**Layer 2 — output (during streaming) — deliberately deprioritized, not currently planned**

Originally specced as: scan `StreamChunk`s as `handleStream` relays them, in parallel with the ongoing stream, abort + show a ToS violation message if flagged mid-stream — a backstop against jailbreak patterns and unwanted output that wasn't obvious from the request alone.

Revisited and dropped for now, not just left undone:

- The catalog only routes through models that already carry their own vendor-side safety tuning (see "OpenRouter integration"/`configs/models.json`) — the marginal case Layer 2 catches is narrow: a jailbreak that both slipped past Layer 1's input check *and* got a frontier model to comply anyway, not the general "is this message OK" question Layer 1 already answers.
- Real cost, not a rounding error: a second moderation-model call per request (this time against streamed output, so either buffered-and-rescanned per chunk or accepting extra latency), on top of Layer 1's existing classify+moderate calls.
- Same reasoning `docs/unit-economics.md` section 6 already used to drop the 5h/7d spend-limit sub-windows (see "Spend limits" below): building it now means sizing thresholds/categories against guesses, not real incident data. Revisit if Layer 1 block logs (`moderation.BlockLog`) or a real incident show jailbreaks are actually getting through in practice — that's the trigger, not a calendar date.
- The one real gap this leaves: no server-side backstop if a model *does* produce something bad despite Layer 1 passing it and the vendor's own tuning missing it. Cheapest mitigation available today without building Layer 2: retroactively re-run Layer 1's moderation check against `conversation.Message`s already persisted by `finalize`, for logging/audit rather than real-time blocking — not implemented either, just cheaper to add later than the streaming version if it turns out to matter.

**Architectural requirements:**

- Layer 1's classify+moderate check is an async goroutine pair (`sync.WaitGroup`), never a sequential step — see `server.prepare`.
- Log intended vs. actual outcome, and which layer triggered the block — needed for debugging and threshold calibration. Layer 1 records every block via `moderation.BlockLog` (`user_id`, `categories`, `reason`, `timestamp`) in addition to the `log.Printf` line — see below.

### Streaming

`POST /chat/stream` delivers a reply incrementally over Server-Sent Events instead of one JSON body, sitting alongside (not replacing) `POST /chat` — same request shape, same pipeline (`server.prepare` -> route+generate -> `server.finalize`), just a different response transport.

- **`provider.StreamingClient`** is a second, optional interface (`GenerateStream(ctx, apiModelID, messages) (<-chan StreamChunk, error)`) alongside `Client.Generate` — not every `Client` needs it (classifier/moderation only ever consume one parsed JSON reply), so it's a separate interface rather than a new `Client` method every implementation would have to grow a no-op version of. `provider.OpenRouterClient`, `provider.FakeClient`, and `provider.CircuitBreakerClient` (delegating to its wrapped client, plus the same open-circuit fast-fail and failure/success bookkeeping `Generate` does) all implement it, so every `Generators` entry in `cmd/server/main.go` supports streaming automatically, no wiring changes needed there.
- **`StreamChunk`** carries either an incremental `Delta`, or a terminal chunk: `Done` + `Final` (the completed `GenerateResult`, with real token usage) on success, or `Err` on failure. The channel always closes right after that terminal chunk.
- **`server.handleStream`** shares `prepare`/`routeAndCall`/`finalize` with `handle` (see `server/server.go`) — `routeAndCall` is parameterized over a `generateCaller`, so the circuit-breaker failover loop (excluded-model retry, same as non-streaming) is identical code for both paths, not a duplicate reimplementation. `handleChatStream` translates `handleStream`'s `send(event, payload)` calls into `event: ...\ndata: ...\n\n` SSE frames. Event types: `meta` (a model was just picked, fires again on every failover retry so a client always knows which model is actually generating), `delta` (one text chunk), `done` (the final `chatResponse`, same shape `/chat` returns), `blocked` (Layer 1 moderation flagged the request), `error`.
- Layer 2 (output) moderation would consume exactly this incremental stream to scan against, but is deliberately deprioritized rather than pending — see "Moderation" above.

### Chat personas (system prompt picker)

The model's system prompt — the instructions that shape tone/style for the user actually chatting, distinct from the classifier's and moderator's own system prompts — is no longer a single fixed string. There are five named personas, meant to be a picker in the (not yet built) UI:

**Default, Expert, Friendly, Cynical, Direct**

- **`server.SystemPromptNames`** is the fixed, ordered list of the five names above — the single source of truth both the request validator and the loader use, so adding/renaming a persona is a one-line change in one place.
- **One prompt file per persona:** `prompts/Default.md`, `prompts/Expert.md`, `prompts/Friendly.md`, `prompts/Cynical.md`, `prompts/Direct.md`. All five currently exist as empty placeholders — the prompt text itself hasn't been written yet, on purpose, so this is committed as pure plumbing.
- **`server.LoadSystemPrompts(dir)`** loads all five from `dir/<Name>.md` into a `map[string]string`, called once at startup (`cmd/server/main.go`). Unlike `classifier.LoadSystemPrompt`/`moderation.LoadSystemPrompt` (both still fatal on a missing/empty file — those prompts are load-bearing from day one), a missing or empty persona file here is **not** an error: it's just skipped, logged, and selecting that persona later sends no system message at all instead of an empty one. This is what lets the plumbing ship before the prompt text exists.
- **`chatRequest.persona`** (optional, `json:"persona,omitempty"`) picks one by exact name. Empty defaults to `"Default"`. A non-empty value that isn't one of the five names is rejected with 400 before any classify/moderate/route/generate work happens — validated against the fixed `SystemPromptNames` list, not against which personas currently have prompt text, so a not-yet-written persona is still a legal selection (see above), just a typo isn't.
- Applies identically to both `/chat` and `/chat/stream` — persona resolution happens once in `server.prepare`, shared by both pipelines (see "Streaming" above).
- **Known gap, deliberately out of scope here:** there's no UI yet to actually offer this picker (see README "Additional architectural risks" and "Next steps") — this is the server-side half only, a stand-in until the frontend exists to expose it. `prompts/chat_system_prompt.md`, the single-prompt file from before this change, is now unused and left in place rather than deleted, in case its content is worth folding into one of the five personas (most likely `Default.md`) once real prompt text goes in.

### Spend limits

Full design and the numbers behind it: [`docs/unit-economics.md`](docs/unit-economics.md) section 6. Implemented in the `limits/` package.

- **Two independent pools, not one budget.** Instant is near-free ($0.0014/request) and is never fully blocked — even a locked-out user keeps a working chat. Thinking+Max is the actual constrained resource and the only one that gets hard-capped.
- **One 30-day rolling cap per plan** (not a fixed calendar month — a rolling window closes the boundary-doubling gap where a user could burn the full cap on day 30 and again on day 1). Set *below* the plan price on purpose (Pro $10, Pro+ $50, Max $100 against $19/$100/$200 prices), so the worst case still guarantees real margin, not just break-even. A separate, smaller Instant cap ($3/$5/$8) exists purely as an anti-bot ceiling — it's not a routing decision, so it never reaches `router.Route`. **Wired into `server.prepare` as of 2026-08-13** (`limits.CheckInstantOverCap`, previously defined but never called from the request pipeline — see `audit.md` finding #2): checked before classify/moderate run at all, rejecting the request outright (`server.ErrInstantCapExceeded`, HTTP 429) rather than the queue/delay behavior originally sketched in `docs/unit-economics.md` section 6.5 — there's no request queue in this repo to delay into.
- **`limits.SpendStore`** is the interface seam between this accounting logic and wherever spend actually lives (`Sum`/`Record` over rolling windows). **Redis-backed as of 2026-08-12** (`limits.RedisSpendStore`, wired into `cmd/server`) — one key per `(userID, pool, hourly bucket)`, `INCRBYFLOAT` on `Record`, a TTL so old buckets expire on their own. `limits.InMemorySpendStore` (mutex-guarded map, same hourly-bucket scheme) still exists and backs the root router-only demo (`main.go`) and unit tests that don't need a real Redis.
- **`router.Route` takes a `thinkingMaxLocked bool`**, computed by `limits.CheckThinkingMaxLock` before routing. When true, it forces Thinking/Max down to Instant on the scored path, and rejects (rather than silently substitutes) a thinking/max-tier `manual_model_id` — manual mode is explicitly the loophole that would otherwise let a locked-out user keep hitting an expensive model directly.
- A three-layer version (5h/7d/30d rolling windows, for burst smoothing and front-loading protection) was designed and deliberately dropped for v1: typical usage already sits 2–2.3× below the monthly cap, so the sub-windows would mostly protect against the same abuse the monthly cap already catches, at real implementation cost and with fraction guesses that had no usage data behind them yet.

---

## Pre-launch engineering checklist

Things to bake in now, because retrofitting them later on a live prod system is either expensive, requires a data migration, or breaks backward compatibility.

1. **Model catalog as data, not code.** No hardcoded model list. Models, weights, prices, provider-specific params (e.g. reasoning) live in the same config system as router weights. Otherwise adding model #16 requires a code deploy instead of a config edit — and the catalog will churn regularly.
2. **Provider abstraction layer.** Don't couple the codebase to OpenRouter's response format. If part of the model roster eventually moves to direct vendor contracts, you need an internal unified interface (`ModelProvider` with `Stream()`, `Classify()`, etc.) behind which OpenRouter, direct Anthropic/OpenAI APIs, or anything else can sit. Skipping this turns a hybrid-sourcing migration into a rewrite.
3. **Request-level idempotency and cancellation.** If a user disconnects mid-generation (closes the tab), does the upstream request keep running and burning money? Needs explicit cancellation of the upstream call on client disconnect — `context` cancellation in Go is nearly free if wired in from the start, expensive to retrofit through the whole stack later.
   - **Cancellation: verified, not just assumed.** `handleChat` passes `r.Context()` (net/http cancels this automatically when the client disconnects) straight through `handle` into `Classifier.Classify`, `Moderator.Moderate`, and `gen.Generate` — no `context.Background()` substitution anywhere in between. `provider.OpenRouterClient.Generate` builds its request with `http.NewRequestWithContext`, so the real outbound HTTP call is the piece that actually has to respect this, not just an in-process channel. Three tests pin this down instead of leaving it as an assumption: `provider.TestOpenRouterClient_Generate_RespectsContextCancellation` (a real `httptest.Server` handler that hangs until the client gives up — a regression here would time the test out, not silently pass) plus `server.TestHandle_ClassifyAndModerateRespectContextCancellation` / `TestHandle_GenerateRespectsContextCancellation` (using a new `provider.FakeClient.Block` field that blocks on `ctx.Done()`, added for exactly this). **Idempotency: implemented (`idempotency/`), wired into `server.handle`/`handleStream`.** `chatRequest.idempotency_key` (optional) lets a client mark a retry as "the same attempt" instead of a new one: `server.reserveIdempotent` claims `(user_id, idempotency_key)` via `idempotency.Store.Reserve` before any classify/moderate/route/generate work happens, and `server.finishIdempotent` reports the outcome afterward — a successful response is cached (`Store.Complete`) so a retry with the same key replays it verbatim instead of calling `gen.Generate` (and `limits.RecordInstantSpend`/`RecordThinkingMaxSpend`) a second time, while a failed attempt releases the key (`Store.Release`) so a legitimate retry after a real failure isn't stuck behind `idempotency.ErrInFlight`. A concurrent/retried request that arrives while the original is still in flight is rejected outright rather than raced. **Redis-backed as of 2026-08-12** (`idempotency.RedisStore`, wired into `cmd/server`) — a JSON envelope per `(user_id, key)` distinguishing in-flight from completed, `SET NX` for the claim, an `IDEMPOTENCY_TTL`-bounded TTL on the completed record (default 24h; `idempotency.InMemoryStore` kept entries forever, which a real deployment can't afford). `idempotency.InMemoryStore` still exists and backs unit tests that don't need a real Redis. Empty `idempotency_key` (or no `Idempotency` store configured) disables the guard entirely, so this is opt-in per request, not a breaking change to the wire format.
4. **Circuit breaker / per-model health tracking**, not just a per-request timeout fallback. If a specific model at a specific provider starts failing en masse, the router should exclude it for all users temporarily, rather than every individual request re-hitting the same timeout. This state ("which models are healthy right now") should live in memory/Redis, updated from real failures — without it, the first real incident means every user waits out the same timeout individually instead of a silent automatic bypass.
   - **Done, including failover: `provider.CircuitBreakerClient`** (`provider/circuit_breaker.go`) wraps any `provider.Client` and, per `apiModelID`, fast-fails with `CircuitOpenError` after `FailureThreshold` consecutive failures instead of calling the vendor again, for `CooldownPeriod` before trying that model again. `cmd/server/main.go` wraps the shared `OpenRouterClient` with one (5 failures / 30s cooldown) and uses it for every catalog provider in `Generators` — state is keyed by model, so one breaker instance correctly isolates each model's health even though they all share the same underlying client. `router.Route` now takes an `excludedModelIDs map[string]bool` parameter; `server.handle` catches `CircuitOpenError` from `Generate`, adds the failed model to that set, and retries `Route` (bounded by `maxCircuitFailoverAttempts`, currently 3) so the request lands on a healthy alternative instead of just failing — manual mode is the one exception, since there's no other candidate to substitute for a model the user asked for by name, so an excluded `manual_model_id` still errors out. Health state lives in-process only (no Redis) — unlike the stores below, per-model health doesn't need to survive a restart or be shared across instances to do its job, so this hasn't been migrated and isn't blocked on anything.
5. **Conversation storage schema built for migration from day one.** The chat history format (JSON fields, message schema version) should allow new fields without breaking changes — e.g. storing reasoning blocks separately from the final answer, or metadata like which model answered a given message (needed for UI like "Thought for N seconds"). A single raw-text column saves time now, costs weeks later when you need to backfill metadata onto old records.
   - **Implemented (`conversation/`), wired into `server.handle`.** `chatRequest.conversation_id` threads a request onto stored history; empty starts a new one (`conversation.NewID`, 16 random bytes hex-encoded, no external UUID dependency) and `chatResponse.conversation_id` always returns which thread the turn landed on. Before generating, `handle` loads the full stored history via `conversation.Store` and sends it to the model ahead of the new message — this is the actual "which model answered a given message" example in the flesh: `conversation.Message.ModelID` is set on every stored assistant turn. **Postgres-backed as of 2026-08-12** (`conversation.PostgresStore`, wired into `cmd/server`) — `conversation_messages` (message-per-row, keyed on `(user_id, conversation_id)`, ordered by insertion) plus `conversation_summaries`. `conversation.InMemoryStore` still exists and backs unit tests that don't need a real Postgres. ~~Known limitation, not yet addressed: `estimated_context_tokens` is still whatever the client supplies~~ — fixed: `server.prepare` now computes it itself via `tokenizer.EstimateMessages` against the real system prompt + stored history + new message, instead of trusting the client-supplied number (see "Tokenizer / context estimation" below). Classification and moderation still only look at the newest message, not full history — a deliberate scope cut, not an oversight (see `server.handle`'s doc comment).
6. **Stateless app layer**, ready for horizontal scaling even with a single server today. If session state lives in process memory instead of Redis/DB, the moment you add a second instance, a user landing on the other instance loses context. Session state and rate-limit counters belong in Redis from the start, not local process memory.
   - **Done as of 2026-08-12.** Every store this item worried about is now Redis/Postgres-backed and wired into `cmd/server` — `limits.SpendStore` (Redis), `idempotency.Store` (Redis), `conversation.Store`/`moderation.BlockLog`/`costlog.Store` (Postgres) — see "Local dev infrastructure" in Status below. A second `cmd/server` instance against the same docker-compose Postgres/Redis would already share state correctly; nothing left in `server.Server` lives in local process memory.
7. **Per-request cost accounting, not aggregate.** Log `cost_usd` on every individual API call (classification and generation both), tied to `user_id`, `model`, `timestamp`, from day one. Needed for billing (credit/limit checks), future unit economics, and eval sets. Logging only success/failure without cost is data that's often impossible to reconstruct retroactively — provider pricing changes over time.
   - **Router-side pricing math is in place** (`router/cost.go`): `RouteResult.EstimatedCostUSD` prices the selected model against `estimated_context_tokens` + the classifier's `estimated_output_tokens`, for abuse-defense checks and UI display before generation starts. `NewCostLogEntry` builds the actual per-request billing record from real token usage once a generation completes (`user_id`, `request_id`, `model_id`, `mode`, token counts, `cost_usd`, `timestamp`).
   - **Generation-call persistence: done** (`costlog/`, wired into `server.finalize`) — every completed generation logs one `router.CostLogEntry` via `costlog.Store`, best-effort the same way `Conversations.Append` is (a logging failure doesn't fail the request, since the user already has their answer — it just costs one missing row, not the rolling-window spend `limits.Store` still records correctly for the Thinking+Max cap). **Postgres-backed as of 2026-08-12** (`costlog.PostgresStore`, wired into `cmd/server`) — one row per completed generation in `cost_log`. `costlog.InMemoryStore` still exists and backs unit tests that don't need a real Postgres. `RequestID` is a fresh random ID minted per generation attempt in `server.prepare` (same scheme as `conversation.NewID`), distinct from `conversation_id` (spans a whole thread) and `IdempotencyKey` (client-supplied, optional).
   - **Classifier and moderation calls: now logged too (2026-08-12).** `classifier.Classify`/`moderation.Moderate` now return a `*provider.GenerateResult` alongside their parsed output — non-nil (real billed usage) whenever the underlying vendor call completed, even if a downstream JSON-parse failure means the parsed output itself is unusable; nil only when the vendor call never completed at all. `server.prepare` logs one `cost_log` row per call right after both finish (`server.recordAuxCostLog`, `mode` = `"classify"`/`"moderate"`) — unconditionally, before checking whether moderation flagged/errored or classification failed, since both calls are already billed by that point regardless of outcome. Priced via `Classifier.CostInputPerMTok`/`CostOutputPerMTok` and `Moderator`'s equivalents (new `router.ComputeCostUSDRates`, since neither model necessarily has a catalog entry — moderation's default, `gpt-oss-120b`, deliberately doesn't) — set in `cmd/server/main.go` from `CLASSIFIER_COST_*_PER_MTOK`/`MODERATION_COST_*_PER_MTOK` env vars, defaulting to `docs/unit-economics.md`'s assumed figures for the `*_API_MODEL_ID` defaults. All three of one `/chat` call's billed model calls (classify, moderate, generate) now share one `RequestID`, so they reconcile as a group. `docs/unit-economics.md`'s classifier/moderation cost figures are still model-level *estimates* used for the unit-economics analysis itself — this closes the gap between those estimates and what actually gets billed per request, it doesn't replace the doc.
8. **Secrets/API keys via env/vault from the first commit.** With 8+ providers/keys (OpenRouter, possibly direct contracts later, email, payment processor), hardcoded secrets on live prod are a standing leak/downtime risk waiting to happen.

**Highest priority for a solo MVP:** #1, #3, #6, #7 — model catalog as data, disconnect cancellation, statelessness via Redis, per-request cost logging. The rest (circuit breaker, provider abstraction, schema versioning) matter but can wait without catastrophic consequences if you can't get to everything in week one.

## Additional architectural risks

1. **OpenRouter as a single point of failure.** Model-to-model fallback doesn't help if the whole OpenRouter platform goes down. Need at least 1–2 direct vendor contracts (Anthropic, OpenAI) as an emergency platform-level fallback.
2. **Prompt caching vs. routing conflict.** Vendor-side context caching gives massive savings on long conversations, but breaks when the model changes mid-session. Rule needed: don't switch models within a session without good reason, or explicitly accept the loss of caching savings as the price of precise routing.
3. **Context window mismatch.** Switching to a model with a smaller context window may not fit the conversation history. The router treats a candidate's context size as a hard filter (`router.Route`'s `estimatedContextTokens` parameter), fed a real server-computed estimate instead of an unverified client-supplied one (see "Tokenizer / context estimation" above). `server.prepare` also now folds the older part of the conversation into a rolling summary (`summarizer/`, `conversation.Store.GetSummary`/`SetSummary`) once the full history's estimate crosses `Server.SummaryTriggerTokens`, keeping only that summary plus the last `Server.SummaryTailMessages` verbatim — so a genuinely long conversation stays within a candidate model's window instead of just failing the hard filter outright. Summarization itself calls a cheap model (same one as the classifier by default) and can fail; on failure `prepare` falls back to the full untrimmed history, i.e. today's pre-summarization behavior, so the worst case is unchanged. Full history is never deleted from `conversation.Store` — only what's sent to the model per turn is trimmed.
4. **Prompt injection against the system prompt.** Users will try to extract the system prompt and figure out which model is actually answering. Not a security issue per se, but it undermines the "don't think about models" positioning if it becomes common knowledge.
5. **Cost-based abuse.** A single request with a deliberately bloated context can blow past a dollar limit in one shot. Cost must be estimated from input token count *before* sending, not only after the fact. **Partially addressed, 2026-08-13** (see `audit.md`): `provider.OpenRouterClient.MaxTokens` now caps every generation call's *output* side too (`OPENROUTER_MAX_TOKENS`, default 16000), so a single call can no longer run all the way to a catalog model's own `max_output_tokens` ceiling (128000+); `decodeChatRequest` now rejects request bodies over `maxRequestBodyBytes` (64KB) before decoding; and `server.Server.IPRateLimiter` throttles POST `/chat`/`/chat/stream` per client IP (`RATE_LIMIT_PER_MINUTE`, default 60/min), independent of the (still unauthenticated, see item below and `audit.md` finding #1) client-supplied `user_id`. None of this replaces real authentication — an attacker who spreads requests across many source IPs and fabricated `user_id`s is still only slowed down, not stopped.
6. **Onboarding and cognitive load.** 5 modes (Auto/Instant/Thinking/Max/Manual) can overwhelm a new user, even though the whole point of the product is removing the burden of choice. Needs a default mode (Auto) on first visit, explained through UX, not a text instruction.
7. **API key exposure.** A compromised server means the entire budget across 8+ providers is at risk simultaneously. Beyond env/vault storage, per-provider spending caps on the OpenRouter/vendor side are needed as an external safety net.

---

## Infrastructure (MVP)

*Note: table figures may be stale — verify against current provider pricing.*

| Item | Spec | Price/mo | Why |
|---|---|---|---|
| **App server** | Hetzner CX33 — 4 vCPU / 8 GB / 80 GB SSD | €6.49 | Runs the backend, Postgres, Redis, and the Go router. I/O-bound load (proxying to OpenRouter + two sequential API calls per request), not CPU-bound — no heavy local inference; classification and model selection happen in external APIs and lightweight Go logic. |
| **IPv4 address** | required add-on | €0.50 | Without it the server has no public IPv4; IPv6-only cuts off part of clients/integrations. |
| **Automated backups** | +20% of server price, daily snapshots | €1.30 | Insurance against data loss from a bad deploy — the only safety net at the single-server-no-replica stage. |
| **Object storage (S3)** | Hetzner, base tier: 1 TB storage + 1 TB egress | €4.99 | Stores user chat files/images, S3-compatible API — same pattern as AWS S3, without the brand markup. |
| **.sh domain** | ~$30–45/yr | ~€3–4 | Annual, not monthly; brand fit for a product with an explicit model-choice technical layer. |
| **Transactional email** | Resend/Postmark, free tier up to ~3000 emails/mo | €0 | Magic links, password reset — volume won't exceed the free tier at MVP scale. |
| **CDN/WAF** | Cloudflare free tier | €0 | Baseline DDoS protection in front of the server, static caching — no need to self-host this. |
| **Error tracking** | Sentry developer tier | €0 | Crash/exception tracking in prod — free event limit covers MVP traffic. |
| **Uptime monitoring** | UptimeRobot free / Hetzner built-in metrics | €0 | Alerts if the server goes down — without it you find out from users, not before them. |

**Payments:** platega.io / NOWpayments

---

## Status

- Positioning and mode structure defined.
- Routing architecture and config system designed (config-driven weights, model catalog as data, classifier output schema).
- Go router: implemented — feature-based scoring against `weights.json`, hard filters, manual-mode passthrough, auto-mode tier selection, deterministic cost-tie breaking, per-request cost estimation (`router/cost.go`), a spend-lock override (`thinkingMaxLocked`) that forces Thinking/Max down to Instant, and an `excludedModelIDs` parameter that `server.handle` uses to fail over around a model whose circuit just opened (see pre-launch checklist item 4). No real persistence yet (by design, deferred — see `limits/`).
- Spend limits: designed and implemented (`limits/`, see "Spend limits" above) — 30-day rolling hard cap per plan, Redis-backed (`limits.RedisSpendStore`) as of 2026-08-12.
- Unit economics: modeled (`docs/unit-economics.md`) — typical ~81–86% margin, guaranteed-margin worst case via the spend cap above.
- Classifier: prompt written and validated (`prompts/classifier_system_prompt.md`) against live Gemini 3.5 Flash-Lite / Claude Haiku 4.5 calls, and wired into `cmd/server` for automatic use per request.
- Chat system prompts: mechanism implemented as five named personas (see "Chat personas" above) — `server.SystemPromptNames` (Default/Expert/Friendly/Cynical/Direct), `server.LoadSystemPrompts` loading `prompts/<Name>.md`, `chatRequest.persona` selecting one (400 on an unrecognized name, "Default" when unspecified). All five prompt files are currently empty placeholders — the actual prompt text isn't written yet, and no UI exists yet to expose the picker to a user.
- OpenRouter integration: written (`provider.OpenRouterClient`), wired into `cmd/server`. **Confirmed live (2026-08-09):** a local run reached OpenRouter and got real HTTP responses back (429/401), proving the request/auth wiring is correct end to end — no successful generated response captured yet (blocked on account balance/rate limit, not code). See [`docs/running-locally.md`](docs/running-locally.md).
- Model catalog (`configs/models.json`): every entry now carries a real OpenRouter slug (`api_model_id`) and pricing/context-window figures looked up against OpenRouter's model pages (2026-08-09; this session's own network egress to `openrouter.ai` is blocked, so this was done via indexed-page search rather than a direct fetch — treat as a strong first pass, not a substitute for one confirmed live generation per model). `cmd/server/main.go`'s `Generators` map now routes all five catalog providers (`anthropic`, `openai`, `google`, `moonshot`, `deepseek`) through the same `OpenRouterClient`. **Not yet re-verified:** `kimi-k3` and `deepseek-v4-flash`'s `max_output_tokens` (search results for these were inconsistent/implausible, so the prior placeholder was kept rather than overwritten with an unreliable number). `docs/unit-economics.md`'s blended-rate tables and worked examples have been recomputed (2026-08-11) against the current `claude-sonnet-5` ($2/$10), `kimi-k3` ($2.80/$14), `gpt-5.6-luna` ($0.10/$0.60), and `gpt-5.6-terra` ($1.0/$6.0) prices. The price drops changed which model actually wins `scoreAndPick` in two tiers, not just the raw numbers: `gpt-5.6-terra` is now the cheapest thinking-tier model outright (undercutting `claude-sonnet-5`/`kimi-k3` on price while matching or exceeding their tools/modality/context_window support, so auto-routing picks it almost every time), and `gpt-5.6-luna` is now cheap enough to win the instant tier for any tool-requiring request, displacing `gemini-3.5-flash-lite`/`claude-haiku-4-5` from that share of the mix. Typical margin is now ~81–86% (was ~75–80%) — see `docs/unit-economics.md` sections 1–2 and 5. **`gpt-oss-120b` removed from the catalog (2026-08-11)** — a product call after hands-on testing, not a routing/price consideration; it had been the cheapest no-tool instant model and the default auto-routing pick for that share of traffic. Catalog is now 11 entries, not 12. The model stays the default for **moderation** (`MODERATION_API_MODEL_ID`, unrelated role — a short JSON verdict, not a user-facing reply) — only its catalog listing for chat generation was removed. `deepseek-v4-flash` absorbed its entire no-tool instant share (it already covered the large-context slice; without `gpt-oss-120b` it's simply the cheapest instant model with no tools required, regardless of context size) — see `docs/unit-economics.md`'s updated instant-tier mix. Net effect on typical margin: negligible (~$0.01–0.02/mo per plan, rounds to the same ~81–86%).
- Moderation: Layer 1 (input) implemented (`moderation/`, wired into `server.handle`/`handleStream`, see "Moderation" above) — runs concurrently with the classifier, blocks generation outright on a flag, fails closed on a moderation-model error, and persists every block via `moderation.BlockLog` (Postgres-backed as of 2026-08-12, `moderation.PostgresBlockLog`). Layer 2 (output, streamed) deliberately deprioritized, not planned unless Layer 1's block logs or a real incident show it's actually needed — see "Moderation" above.
- Streaming: implemented — `POST /chat/stream` (see "Streaming" above), `provider.StreamingClient` on `OpenRouterClient`/`FakeClient`/`CircuitBreakerClient`, `server.handleStream` sharing `prepare`/`routeAndCall`/`finalize` with the non-streaming `handle`.
- Circuit breaker: `provider.CircuitBreakerClient` implemented and wraps the shared `OpenRouterClient` in `cmd/server/main.go` (5 consecutive failures / 30s cooldown, per-`apiModelID` state) — see pre-launch checklist item 4. Stops a dead model from being retried by every request, and `server.handle` now reroutes around an open circuit to a healthy alternative via `router.Route`'s `excludedModelIDs`, bounded by `maxCircuitFailoverAttempts`.
- Conversation storage: implemented (`conversation/`, wired into `server.handle`, see pre-launch checklist item 5) — `/chat` now threads requests onto a `conversation_id` and sends stored history to the model, not just the latest message in isolation. Postgres-backed as of 2026-08-12 (`conversation.PostgresStore`). Classification/moderation still only see the newest message — a documented, deliberate limitation, not fixed by this.
- Tokenizer / context estimation: implemented (`tokenizer/`, wired into `server.prepare`, see "Tokenizer / context estimation" above) — `estimated_context_tokens` is now computed server-side from the real message list on every turn instead of trusted from the client, closing both the "Context window mismatch" drift and the "Cost-based abuse" understating loophole. Heuristic approximation, not a real per-vendor BPE tokenizer — deliberate, see that section for why.
- Long-conversation summarization: implemented (`summarizer/`, wired into `server.prepare` via `Server.maybeSummarize`, see "Context window mismatch" above) — once a conversation's full stored history estimate crosses `Server.SummaryTriggerTokens` (default: 60% of the smallest catalog model's `ContextWindow`, `SUMMARY_TRIGGER_TOKENS` env override), the older part gets folded into a rolling `conversation.Summary` and only that summary plus the last `Server.SummaryTailMessages` (`SUMMARY_TAIL_MESSAGES` env override, default 10) go to the model, instead of the full unbounded history. `conversation.Store` gained `GetSummary`/`SetSummary`, backed by the `conversation_summaries` table (Postgres, see above). `conversation.Message` also gained an `IsSummary bool` field, unused for now — reserved for telling synthetic summary content apart from real turns once something needs to (e.g. excluding it from a transcript export). `prompts/summarizer_system_prompt.md` is currently an empty placeholder — `cmd/server/main.go` will fail to start (`log.Fatal`) until it's filled in, same as any other missing/empty required prompt file.
- Request cancellation: verified with tests, not just assumed — see pre-launch checklist item 3.
- Idempotency: implemented (`idempotency/`, wired into `server.handle`/`handleStream`, see pre-launch checklist item 3 and "Next steps") — a client retry carrying the same `idempotency_key` replays the original response instead of triggering a second generate call and a second charge. Redis-backed as of 2026-08-12 (`idempotency.RedisStore`).
- Per-request cost logging: generation calls implemented (`costlog/`, wired into `server.finalize`, see pre-launch checklist item 7) — one `router.CostLogEntry` per completed generation. Postgres-backed as of 2026-08-12 (`costlog.PostgresStore`). Classifier/moderation calls still not logged per-request (their functions don't return token usage yet).
- **Local dev infrastructure (Docker + Postgres + Redis): done (2026-08-12).** All five throwaway `InMemory*` stores (`limits`, `moderation`, `conversation`, `idempotency`, `costlog`) now have real Postgres/Redis-backed implementations wired into `cmd/server` — see the individual bullets above and each package's "Postgres-backed"/"Redis-backed" note. `docker-compose.yml` at repo root runs `postgres:16-alpine` + `redis:7-alpine` (sized for a 2 GB dev host: `shared_buffers=32MB`/`max_connections=20`/`effective_cache_size=128MB` on Postgres, an explicit `maxmemory`/`maxmemory-policy` on Redis, `mem_limit` on both). Migrations are plain SQL files under `db/migrations/`, embedded via `go:embed` and applied automatically by `cmd/server` on startup (`db.Migrate`) against a `schema_migrations` tracking table — no manual migration step in the normal case. Secrets (Postgres/Redis credentials, `OPENROUTER_API_KEY`) load from a gitignored `.env` via a small dependency-free loader (`internal/envfile`); `.env.example` documents every variable. `InMemory*` versions of all five stores still exist and back unit tests / the root router-only demo (`main.go`) that don't need a real Postgres/Redis. See [`docs/running-locally.md`](docs/running-locally.md) for the setup steps and manual verification queries.
- **`cmd/server` itself now containerized too (2026-08-12).** A multi-stage `Dockerfile` (Go build stage → `alpine:3.20` runtime, non-root user, `GET /health` `HEALTHCHECK`) builds `cmd/server` and bakes in `configs/`/`prompts/`. `docker-compose.yml`'s `server` service depends on `postgres`/`redis` reaching `service_healthy` before starting, and overrides `POSTGRES_HOST`/`REDIS_HOST` to the compose-network service names (`.env`'s `127.0.0.1` defaults stay correct for the host-`go run` path in `docs/running-locally.md`'s "Running without Docker"). `docker compose up -d --build` now brings up all three services and the server starts serving `/chat` without a separate `go run` step — verified end to end against real containers: migrations applied, `/health` returns 200, `/chat` reaches through classify → moderate → OpenRouter (401 with a placeholder key, proving the whole path executes, not just that the binary starts).

## Next steps (execution)

1. ~~Wire up real OpenRouter calls, or at minimum a first direct-vendor call~~ — done and confirmed live: `cmd/server` runs classify → check spend lock → route → generate → record spend end to end against OpenRouter (`provider.OpenRouterClient`).
2. ~~Get one fully successful generation (not just a real HTTP response) once the test account's balance/rate limit allows it~~ — done (2026-08-12): a real free-tier OpenRouter key/model (`openai/gpt-oss-20b:free`) ran the whole pipeline against the containerized `cmd/server` — classify → moderate → route (`manual` mode, since the free model has no paid catalog entry) → generate — and produced a real generated reply, persisted to `conversation_messages` and `cost_log`. Confirms the classifier's JSON reply does round-trip through a real model over OpenRouter, not just `FakeClient`-scripted responses. The temporary catalog entry and key used for the test were removed afterward (never committed); the key itself was rotated since it had been pasted into a chat transcript.
3. ~~Once there's a deployed server process (not just a local `go run`): replace `limits.InMemorySpendStore` with a Redis/Postgres-backed `SpendStore`~~ — done (2026-08-12), ahead of an actual deployed server: `limits.RedisSpendStore` against the local docker-compose Redis, wired into `cmd/server` (see "Spend limits" and Status's "Local dev infrastructure" bullet). A production deploy still needs its own Postgres/Redis, but the interface swap itself — the part this item was actually about — is done.
4. ~~Replace placeholder pricing/context-window figures in `models.json` with real numbers, and confirm each catalog entry's real OpenRouter slug (`api_model_id`)~~ — done for all 12 entries (2026-08-09), sourced via search since this session can't reach `openrouter.ai` directly; worth a real live-call spot-check per model once possible, and `kimi-k3`/`deepseek-v4-flash`'s `max_output_tokens` still need a real source.
5. ~~Extend `cmd/server/main.go`'s `Generators` map to cover every catalog provider~~ — done: all five provider tags route through `OpenRouterClient`.
6. ~~Recompute `docs/unit-economics.md`'s blended rates, worked examples, and the ~75–80% margin conclusion against the updated `models.json` prices (`claude-sonnet-5`, `kimi-k3`, `gpt-5.6-luna`, and `gpt-5.6-terra` all changed)~~ — done (2026-08-11). Two mix changes fell out of re-checking the hard-filter/scoring logic against the new prices, not just re-plugging numbers: thinking tier is now dominated by `gpt-5.6-terra` ($1/$6, cheapest and capability-equal-or-better vs. `claude-sonnet-5`) instead of `claude-sonnet-5`/`kimi-k3`; instant tier's tool-requiring share moved from `gemini-3.5-flash-lite`/`claude-haiku-4-5` to `gpt-5.6-luna` ($0.10/$0.60, now cheaper with equal-or-better tool support). Also dropped Layer 2 moderation cost from the model (deliberately deprioritized, see item 8) and kept only Layer 1's real cost (`gpt-oss-120b`, ~170 input tokens/request). Typical margin recomputed to ~81–86% (was ~75–80%).
7. Revisit the dropped 5h/7d sub-window layers (see "Spend limits") once real usage data exists to size their fractions on, instead of on guesses.
8. ~~Implement Layer 1 (input) moderation~~ — done (`moderation/`). Layer 2 (output, streamed) deliberately dropped rather than built — see "Moderation" above for why; revisit only if Layer 1's block logs or a real incident show jailbreaks are actually getting through.
9. ~~Persist moderation-block events (`user_id`, `categories`, `reason`, timestamp) to a real store instead of just `log.Printf`~~ — done via `moderation.BlockLog`, and (2026-08-12) folded into the same Postgres migration as item 3's `SpendStore` sibling stores, exactly as anticipated here: `moderation.PostgresBlockLog` against the same local docker-compose Postgres instance as `conversation.Store`/`costlog.Store`.
10. ~~Implement a circuit breaker / per-model health tracking~~ — done, fast-fail and failover both (`provider.CircuitBreakerClient` + `router.Route`'s `excludedModelIDs`, pre-launch checklist item 4): `server.handle` catches `CircuitOpenError` from `Generate` and retries routing with the failed model excluded so the request lands on a healthy alternative instead of just failing. Still deliberately in-process only (unlike items 3/9/11/12's stores) — see the pre-launch checklist item 4 note on why this one doesn't need to move to Redis.
11. ~~Implement conversation storage~~ — done (`conversation/`, pre-launch checklist item 5), and (2026-08-12) `conversation.PostgresStore` folded into the same database migration as items 3/9's stores. ~~(a) `estimated_context_tokens` doesn't grow with real stored history~~ — done (`tokenizer/`, see "Tokenizer / context estimation" above): `server.prepare` now recomputes it from the real message list every turn. ~~(c) no summarization/truncation for conversations that outgrow a candidate model's context window~~ — done (`summarizer/`, see "Context window mismatch" above and "Long-conversation summarization" below): `server.prepare` now folds the older part of a long conversation into a rolling summary once it crosses `Server.SummaryTriggerTokens`, instead of just failing the hard filter outright. Still open: (b) classification/moderation still only see the newest message, never full history, a deliberate scope cut worth revisiting if that assumption turns out wrong in practice (see `server.handle`'s doc comment).
12. ~~Verify request cancellation on client disconnect~~ — done (pre-launch checklist item 3): three tests (`provider.TestOpenRouterClient_Generate_RespectsContextCancellation`, `server.TestHandle_ClassifyAndModerateRespectContextCancellation`, `server.TestHandle_GenerateRespectsContextCancellation`) confirm `ctx` cancellation actually stops an in-flight call rather than letting it run to completion unobserved, at both the real-HTTP layer and the `server.handle` orchestration layer. ~~Idempotency is not done~~ — done (`idempotency/`, see "Request-level idempotency and cancellation" above): a client retry with the same `idempotency_key` now replays the original response instead of causing a second `gen.Generate` call and a second `RecordInstantSpend`/`RecordThinkingMaxSpend`. ~~In-process only~~ — Redis-backed as of 2026-08-12 (`idempotency.RedisStore`), same migration as items 3/9/11.
13. Get a Postgres/Redis backend for a real *production* deploy, distinct from the local docker-compose instances items 3/9/11/12 now use for dev — same interfaces, different (managed/hosted) connection details, once there's an actual server to deploy to. See `docs/running-locally.md` for the local setup these will eventually sit alongside.
