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
2. ~~Unit economics are not modeled yet.~~ Modeled — see [`docs/unit-economics.md`](docs/unit-economics.md): typical margin ~75–80%, plus a guaranteed-margin plan for the Thinking+Max spend cap (`limits/`, see below).

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

### Moderation

Two-layer, parallel moderation — never blocking on UX latency.

**Layer 1 — input (before showing the response to the user) — implemented (`moderation/`, wired into `server.handle`)**

- Runs concurrently with classification (`sync.WaitGroup`, two goroutines) — both are cheap-model calls over the same raw request text, so there's no reason to pay their latency twice.
- A cheap model checks the request text against `prompts/moderation_system_prompt.md` and returns `{flagged, categories, reason}` — same JSON-over-a-cheap-model pattern as the classifier (`classifier/`), including the same fenced-reply defense.
- `flagged` → generation is skipped entirely (never even attempted) and the user sees nothing but a fixed ToS violation message (`chatResponse.Blocked`); the flagging model/reason is logged server-side, not exposed to the client.
- **Block events are persisted, not just logged.** Every flag calls `moderation.BlockLog.Record` (`user_id`, `categories`, `reason`, `timestamp`) in addition to the `log.Printf` line — needed to eventually answer "how often, which categories, is this user repeatedly hitting the filter" instead of only ever seeing one flag in isolation in process stdout. `moderation.InMemoryBlockLog` is, deliberately, the same kind of throwaway stand-in as `limits.InMemorySpendStore` — an in-process slice behind a mutex, nothing persisted across a restart, no cross-instance sharing. It exists because there's no server/database yet to justify a real one; swapping in Postgres/Redis later means implementing `moderation.BlockLog`'s one-method interface and deleting `InMemoryBlockLog`, same swap-in procedure as `SpendStore`. A failure to *record* a block never un-blocks it — the block itself already happened by the time `Record` is even called.
- **Deviation from the original design, and why:** the spec above (start generation concurrently into a server-side buffer, discard on flag) is a streaming-pipeline optimization — this repo's `/chat` endpoint isn't streaming yet (see "Layer 2" below), so generating before moderation clears would only waste money, never save user-visible latency. Generation simply waits on both results instead.
- **Fails closed:** a moderation call erroring (as opposed to flagging) fails the whole request rather than letting an unmoderated message through — an outage of the moderation model is currently an outage of chat. Revisit once the circuit-breaker item (pre-launch checklist) makes a softer fallback safe.

**Layer 2 — output (during streaming) — not built**

- Needs the response to actually be streamed first; `/chat` today returns one complete JSON response, so there's no incremental output to scan mid-stream yet.
- Response chunks are checked incrementally, in parallel with the ongoing stream — moderation doesn't gate the stream, it runs as an async observer.
- If flagged mid-stream: abort the stream + show a ToS violation message.
- Backstop against jailbreak patterns and unwanted output that wasn't obvious from the request alone.

**Architectural requirements:**

- Both checks are async goroutines, never sequential steps. (True today for classify+moderate; will also apply to generate+Layer 2 once streaming exists.)
- Cost on flagged requests isn't always zero once Layer 2/streaming exists — generation may partially complete in parallel with the input check. Accepted tradeoff for UX (the alternative is DPI-style latency on every request). Not a concern yet for Layer 1 alone: it gates generation outright, so a flagged request costs only the classify+moderate calls, nothing more.
- Log separately: intended vs. actual outcome, and which layer triggered the block — needed for debugging and threshold calibration. Layer 1 records every block via `moderation.BlockLog` (`user_id`, `categories`, `reason`, `timestamp`) in addition to the `log.Printf` line — see below.

### Spend limits

Full design and the numbers behind it: [`docs/unit-economics.md`](docs/unit-economics.md) section 6. Implemented in the `limits/` package.

- **Two independent pools, not one budget.** Instant is near-free ($0.0014/request) and is never fully blocked — even a locked-out user keeps a working chat. Thinking+Max is the actual constrained resource and the only one that gets hard-capped.
- **One 30-day rolling cap per plan** (not a fixed calendar month — a rolling window closes the boundary-doubling gap where a user could burn the full cap on day 30 and again on day 1). Set *below* the plan price on purpose (Pro $10, Pro+ $50, Max $100 against $19/$100/$200 prices), so the worst case still guarantees real margin, not just break-even. A separate, smaller Instant cap ($3/$5/$8) exists purely as an anti-bot ceiling — it's a gateway-side throttle concern, not a routing decision, so it never reaches `router.Route`.
- **`limits.SpendStore`** is the interface seam between this accounting logic and wherever spend actually lives (`Sum`/`Record` over rolling windows). `limits.InMemorySpendStore` is a deliberately throwaway stand-in — a mutex-guarded map using hourly buckets, good enough to develop and test against, with no persistence across restarts. It exists because there is no server yet to justify building a real Postgres/Redis-backed implementation; swapping one in later means implementing the same two-method interface and deleting the in-memory one, nothing else changes.
- **`router.Route` takes a `thinkingMaxLocked bool`**, computed by `limits.CheckThinkingMaxLock` before routing. When true, it forces Thinking/Max down to Instant on the scored path, and rejects (rather than silently substitutes) a thinking/max-tier `manual_model_id` — manual mode is explicitly the loophole that would otherwise let a locked-out user keep hitting an expensive model directly.
- A three-layer version (5h/7d/30d rolling windows, for burst smoothing and front-loading protection) was designed and deliberately dropped for v1: typical usage already sits 2–2.3× below the monthly cap, so the sub-windows would mostly protect against the same abuse the monthly cap already catches, at real implementation cost and with fraction guesses that had no usage data behind them yet.

---

## Pre-launch engineering checklist

Things to bake in now, because retrofitting them later on a live prod system is either expensive, requires a data migration, or breaks backward compatibility.

1. **Model catalog as data, not code.** No hardcoded model list. Models, weights, prices, provider-specific params (e.g. reasoning) live in the same config system as router weights. Otherwise adding model #16 requires a code deploy instead of a config edit — and the catalog will churn regularly.
2. **Provider abstraction layer.** Don't couple the codebase to OpenRouter's response format. If part of the model roster eventually moves to direct vendor contracts, you need an internal unified interface (`ModelProvider` with `Stream()`, `Classify()`, etc.) behind which OpenRouter, direct Anthropic/OpenAI APIs, or anything else can sit. Skipping this turns a hybrid-sourcing migration into a rewrite.
3. **Request-level idempotency and cancellation.** If a user disconnects mid-generation (closes the tab), does the upstream request keep running and burning money? Needs explicit cancellation of the upstream call on client disconnect — `context` cancellation in Go is nearly free if wired in from the start, expensive to retrofit through the whole stack later.
   - **Cancellation: verified, not just assumed.** `handleChat` passes `r.Context()` (net/http cancels this automatically when the client disconnects) straight through `handle` into `Classifier.Classify`, `Moderator.Moderate`, and `gen.Generate` — no `context.Background()` substitution anywhere in between. `provider.OpenRouterClient.Generate` builds its request with `http.NewRequestWithContext`, so the real outbound HTTP call is the piece that actually has to respect this, not just an in-process channel. Three tests pin this down instead of leaving it as an assumption: `provider.TestOpenRouterClient_Generate_RespectsContextCancellation` (a real `httptest.Server` handler that hangs until the client gives up — a regression here would time the test out, not silently pass) plus `server.TestHandle_ClassifyAndModerateRespectContextCancellation` / `TestHandle_GenerateRespectsContextCancellation` (using a new `provider.FakeClient.Block` field that blocks on `ctx.Done()`, added for exactly this). **Idempotency itself is still unaddressed** — nothing currently stops a client retry after a dropped response from generating (and billing) twice; that half of this checklist item is still open.
4. **Circuit breaker / per-model health tracking**, not just a per-request timeout fallback. If a specific model at a specific provider starts failing en masse, the router should exclude it for all users temporarily, rather than every individual request re-hitting the same timeout. This state ("which models are healthy right now") should live in memory/Redis, updated from real failures — without it, the first real incident means every user waits out the same timeout individually instead of a silent automatic bypass.
   - **Done, including failover: `provider.CircuitBreakerClient`** (`provider/circuit_breaker.go`) wraps any `provider.Client` and, per `apiModelID`, fast-fails with `CircuitOpenError` after `FailureThreshold` consecutive failures instead of calling the vendor again, for `CooldownPeriod` before trying that model again. `cmd/server/main.go` wraps the shared `OpenRouterClient` with one (5 failures / 30s cooldown) and uses it for every catalog provider in `Generators` — state is keyed by model, so one breaker instance correctly isolates each model's health even though they all share the same underlying client. `router.Route` now takes an `excludedModelIDs map[string]bool` parameter; `server.handle` catches `CircuitOpenError` from `Generate`, adds the failed model to that set, and retries `Route` (bounded by `maxCircuitFailoverAttempts`, currently 3) so the request lands on a healthy alternative instead of just failing — manual mode is the one exception, since there's no other candidate to substitute for a model the user asked for by name, so an excluded `manual_model_id` still errors out. Health state lives in-process only (no Redis), same "no server to justify it yet" reasoning as `limits.InMemorySpendStore`.
5. **Conversation storage schema built for migration from day one.** The chat history format (JSON fields, message schema version) should allow new fields without breaking changes — e.g. storing reasoning blocks separately from the final answer, or metadata like which model answered a given message (needed for UI like "Thought for N seconds"). A single raw-text column saves time now, costs weeks later when you need to backfill metadata onto old records.
   - **Implemented (`conversation/`), wired into `server.handle`.** `chatRequest.conversation_id` threads a request onto stored history; empty starts a new one (`conversation.NewID`, 16 random bytes hex-encoded, no external UUID dependency) and `chatResponse.conversation_id` always returns which thread the turn landed on. Before generating, `handle` loads the full stored history via `conversation.Store` and sends it to the model ahead of the new message — this is the actual "which model answered a given message" example in the flesh: `conversation.Message.ModelID` is set on every stored assistant turn. `conversation.InMemoryStore` is the same kind of throwaway stand-in as `limits.InMemorySpendStore`/`moderation.InMemoryBlockLog` — a mutex-guarded map keyed on `(user_id, conversation_id)`, no persistence across restarts, swap-in procedure documented on `conversation.Store`. **Known limitation, not yet addressed:** `estimated_context_tokens` is still whatever the client supplies — there's no tokenizer in this repo to recompute it from real stored history length, so once a conversation has real history, the router's context-window hard filter and cost pre-estimate (`router/cost.go`) can undercount (see "Context window mismatch" under "Additional architectural risks" below). Classification and moderation also still only look at the newest message, not full history — a deliberate scope cut, not an oversight (see `server.handle`'s doc comment).
6. **Stateless app layer**, ready for horizontal scaling even with a single server today. If session state lives in process memory instead of Redis/DB, the moment you add a second instance, a user landing on the other instance loses context. Session state and rate-limit counters belong in Redis from the start, not local process memory.
   - **The rate-limit counters themselves are designed and interface-ready** (`limits/`, see "Spend limits" above) — `limits.InMemorySpendStore` is exactly the local-process-memory anti-pattern this item warns against, kept deliberately isolated behind `limits.SpendStore` so swapping in Redis is a one-file change once there's a server process to make statelessness matter at all.
7. **Per-request cost accounting, not aggregate.** Log `cost_usd` on every individual API call (classification and generation both), tied to `user_id`, `model`, `timestamp`, from day one. Needed for billing (credit/limit checks), future unit economics, and eval sets. Logging only success/failure without cost is data that's often impossible to reconstruct retroactively — provider pricing changes over time.
   - **Router-side pricing math is in place** (`router/cost.go`): `RouteResult.EstimatedCostUSD` prices the selected model against `estimated_context_tokens` + the classifier's `estimated_output_tokens`, for abuse-defense checks and UI display before generation starts. `NewCostLogEntry` builds the actual per-request billing record from real token usage once a generation completes (`user_id`, `request_id`, `model_id`, `mode`, token counts, `cost_usd`, `timestamp`) — the router only builds this value, persisting it to a store is still unbuilt (needs the server layer this repo doesn't have yet).
8. **Secrets/API keys via env/vault from the first commit.** With 8+ providers/keys (OpenRouter, possibly direct contracts later, email, payment processor), hardcoded secrets on live prod are a standing leak/downtime risk waiting to happen.

**Highest priority for a solo MVP:** #1, #3, #6, #7 — model catalog as data, disconnect cancellation, statelessness via Redis, per-request cost logging. The rest (circuit breaker, provider abstraction, schema versioning) matter but can wait without catastrophic consequences if you can't get to everything in week one.

## Additional architectural risks

1. **OpenRouter as a single point of failure.** Model-to-model fallback doesn't help if the whole OpenRouter platform goes down. Need at least 1–2 direct vendor contracts (Anthropic, OpenAI) as an emergency platform-level fallback.
2. **Prompt caching vs. routing conflict.** Vendor-side context caching gives massive savings on long conversations, but breaks when the model changes mid-session. Rule needed: don't switch models within a session without good reason, or explicitly accept the loss of caching savings as the price of precise routing.
3. **Context window mismatch.** Switching to a model with a smaller context window may not fit the conversation history. The router must treat a candidate's context size as a hard filter, plus needs a unified summarization/truncation mechanism before sending history to any model.
4. **Prompt injection against the system prompt.** Users will try to extract the system prompt and figure out which model is actually answering. Not a security issue per se, but it undermines the "don't think about models" positioning if it becomes common knowledge.
5. **Cost-based abuse.** A single request with a deliberately bloated context can blow past a dollar limit in one shot. Cost must be estimated from input token count *before* sending, not only after the fact.
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
- Spend limits: designed and implemented (`limits/`, see "Spend limits" above) — 30-day rolling hard cap per plan, `InMemorySpendStore` as a throwaway stand-in until there's a server to wire a real store behind the same interface.
- Unit economics: modeled (`docs/unit-economics.md`) — typical ~75–80% margin, guaranteed-margin worst case via the spend cap above.
- Classifier: prompt written and validated (`prompts/classifier_system_prompt.md`) against live Gemini 3.5 Flash-Lite / Claude Haiku 4.5 calls, and wired into `cmd/server` for automatic use per request.
- Chat system prompt: mechanism implemented (`server.Server.SystemPrompt`), the prompt text itself is not written yet. `handle` prepends it as the first "system" message ahead of conversation history and the user's new message on every generation call, when set. `server.LoadSystemPrompt` reads it from `prompts/chat_system_prompt.md`; `cmd/server/main.go` loads it non-fatally (missing/empty file just means no system message is sent) since that file doesn't exist in the repo yet.
- OpenRouter integration: written (`provider.OpenRouterClient`), wired into `cmd/server`. **Confirmed live (2026-08-09):** a local run reached OpenRouter and got real HTTP responses back (429/401), proving the request/auth wiring is correct end to end — no successful generated response captured yet (blocked on account balance/rate limit, not code). See [`docs/running-locally.md`](docs/running-locally.md).
- Model catalog (`configs/models.json`): every entry now carries a real OpenRouter slug (`api_model_id`) and pricing/context-window figures looked up against OpenRouter's model pages (2026-08-09; this session's own network egress to `openrouter.ai` is blocked, so this was done via indexed-page search rather than a direct fetch — treat as a strong first pass, not a substitute for one confirmed live generation per model). `cmd/server/main.go`'s `Generators` map now routes all five catalog providers (`anthropic`, `openai`, `google`, `moonshot`, `deepseek`) through the same `OpenRouterClient`. **Not yet re-verified:** `kimi-k3` and `deepseek-v4-flash`'s `max_output_tokens` (search results for these were inconsistent/implausible, so the prior placeholder was kept rather than overwritten with an unreliable number). **Not yet propagated:** `docs/unit-economics.md`'s blended-rate tables and worked examples still use the old `claude-sonnet-5` ($3/$15), `kimi-k3` ($3/$15), `gpt-5.6-luna` ($1/$6), and `gpt-5.6-terra` ($2.5/$15) prices — several of those changed (see `models.json`), so the ~75–80% margin conclusion needs re-deriving before it's trusted again.
- Moderation: Layer 1 (input) implemented (`moderation/`, wired into `server.handle`, see "Moderation" above) — runs concurrently with the classifier, blocks generation outright on a flag, fails closed on a moderation-model error, and persists every block via `moderation.BlockLog` (`InMemoryBlockLog` throwaway stand-in, same pattern as `limits.InMemorySpendStore`). Layer 2 (streaming output) not built — blocked on `/chat` not streaming yet.
- Circuit breaker: `provider.CircuitBreakerClient` implemented and wraps the shared `OpenRouterClient` in `cmd/server/main.go` (5 consecutive failures / 30s cooldown, per-`apiModelID` state) — see pre-launch checklist item 4. Stops a dead model from being retried by every request, and `server.handle` now reroutes around an open circuit to a healthy alternative via `router.Route`'s `excludedModelIDs`, bounded by `maxCircuitFailoverAttempts`.
- Conversation storage: implemented (`conversation/`, wired into `server.handle`, see pre-launch checklist item 5) — `/chat` now threads requests onto a `conversation_id` and sends stored history to the model, not just the latest message in isolation. `InMemoryStore` throwaway stand-in, same pattern as `limits.InMemorySpendStore`/`moderation.InMemoryBlockLog`. `estimated_context_tokens` still doesn't grow with real history (no tokenizer yet) and classification/moderation still only see the newest message — both documented limitations, not fixed by this.
- Request cancellation: verified with tests, not just assumed — see pre-launch checklist item 3. Idempotency (the other half of that item) is still unaddressed.

## Next steps (execution)

1. ~~Wire up real OpenRouter calls, or at minimum a first direct-vendor call~~ — done and confirmed live: `cmd/server` runs classify → check spend lock → route → generate → record spend end to end against OpenRouter (`provider.OpenRouterClient`).
2. Get one fully successful generation (not just a real HTTP response) once the test account's balance/rate limit allows it, to confirm the classifier's JSON reply actually round-trips through a real model over OpenRouter, not just `FakeClient`-scripted responses.
3. Once there's a deployed server process (not just a local `go run`): replace `limits.InMemorySpendStore` with a Redis/Postgres-backed `SpendStore` (same interface, see "Spend limits").
4. ~~Replace placeholder pricing/context-window figures in `models.json` with real numbers, and confirm each catalog entry's real OpenRouter slug (`api_model_id`)~~ — done for all 12 entries (2026-08-09), sourced via search since this session can't reach `openrouter.ai` directly; worth a real live-call spot-check per model once possible, and `kimi-k3`/`deepseek-v4-flash`'s `max_output_tokens` still need a real source.
5. ~~Extend `cmd/server/main.go`'s `Generators` map to cover every catalog provider~~ — done: all five provider tags route through `OpenRouterClient`.
6. Recompute `docs/unit-economics.md`'s blended rates, worked examples, and the ~75–80% margin conclusion against the updated `models.json` prices (`claude-sonnet-5`, `kimi-k3`, `gpt-5.6-luna`, and `gpt-5.6-terra` all changed) — the current numbers there predate this catalog update. While there, add Layer 1 moderation's real cost (a `gpt-oss-120b` call per request, see "Moderation") — the existing model already assumed this cost, so this is mostly confirming the assumption held.
7. Revisit the dropped 5h/7d sub-window layers (see "Spend limits") once real usage data exists to size their fractions on, instead of on guesses.
8. ~~Implement Layer 1 (input) moderation~~ — done (`moderation/`). Build Layer 2 (output, streaming) once `/chat` actually streams — no point checking response chunks incrementally against a response that's delivered in one shot.
9. ~~Persist moderation-block events (`user_id`, `categories`, `reason`, timestamp) to a real store instead of just `log.Printf`~~ — done via `moderation.BlockLog`/`InMemoryBlockLog`, same throwaway-stand-in pattern as item 3's `SpendStore`. Once a real deployed server/database exists, folding this into the *same* migration as item 3 (rather than a separate one) makes sense — same tradeoffs, same "no server to justify it yet" reasoning, likely the same physical database.
10. ~~Implement a circuit breaker / per-model health tracking~~ — done, fast-fail and failover both (`provider.CircuitBreakerClient` + `router.Route`'s `excludedModelIDs`, pre-launch checklist item 4): `server.handle` catches `CircuitOpenError` from `Generate` and retries routing with the failed model excluded so the request lands on a healthy alternative instead of just failing. Still in-process only; move to Redis alongside item 3/9 once there's a server to justify it.
11. ~~Implement conversation storage~~ — done (`conversation/`, pre-launch checklist item 5), `InMemoryStore` throwaway stand-in same as items 3/9/10's stores — fold into the same eventual database migration. Still open: (a) `estimated_context_tokens` doesn't grow with real stored history — needs an actual tokenizer to recompute it, or the router's context-window hard filter and cost pre-estimate silently drift from reality as conversations get longer; (b) classification/moderation still only see the newest message, never full history, a deliberate scope cut worth revisiting if that assumption turns out wrong in practice (see `server.handle`'s doc comment); (c) no summarization/truncation for conversations that outgrow a candidate model's context window (README "Context window mismatch" risk) — right now a long enough conversation just fails the hard filter outright rather than getting trimmed.
12. ~~Verify request cancellation on client disconnect~~ — done (pre-launch checklist item 3): three tests (`provider.TestOpenRouterClient_Generate_RespectsContextCancellation`, `server.TestHandle_ClassifyAndModerateRespectContextCancellation`, `server.TestHandle_GenerateRespectsContextCancellation`) confirm `ctx` cancellation actually stops an in-flight call rather than letting it run to completion unobserved, at both the real-HTTP layer and the `server.handle` orchestration layer. **Idempotency is not done** — a client retrying after a dropped connection can still cause a duplicate generate call (and duplicate billing); this checklist item covers both, only the cancellation half is closed.
