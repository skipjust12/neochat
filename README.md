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
2. **Unit economics are not modeled yet.** Known: OpenRouter adds a markup on top of vendor pricing, and the plan is to operate at a loss early with no firm cap defined. Risk: burning the budget faster than reaching product-market fit.

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

**Layer 1 — input (before showing the response to the user)**

- Runs in parallel with classification/routing (same 0.5–1s "Routing" overhead, adds nothing on top).
- A cheap model/moderation API checks the request text.
- Generation from the main model starts independently and concurrently, writing to a server-side buffer — nothing is sent to the user yet.
- Once both results (routing + moderation) are back:
  - `ok` → start streaming the buffer to the user, then stream normally.
  - `flagged` → generation is aborted, the user sees nothing but a ToS violation message.

**Layer 2 — output (during streaming)**

- Response chunks are checked incrementally, in parallel with the ongoing stream — moderation doesn't gate the stream, it runs as an async observer.
- If flagged mid-stream: abort the stream + show a ToS violation message.
- Backstop against jailbreak patterns and unwanted output that wasn't obvious from the request alone.

**Architectural requirements:**

- Both checks are async goroutines, never sequential steps.
- Cost on flagged requests isn't always zero — generation may partially complete in parallel with the input check. Accepted tradeoff for UX (the alternative is DPI-style latency on every request).
- Log separately: intended vs. actual outcome, and which layer triggered the block — needed for debugging and threshold calibration.

---

## Pre-launch engineering checklist

Things to bake in now, because retrofitting them later on a live prod system is either expensive, requires a data migration, or breaks backward compatibility.

1. **Model catalog as data, not code.** No hardcoded model list. Models, weights, prices, provider-specific params (e.g. reasoning) live in the same config system as router weights. Otherwise adding model #16 requires a code deploy instead of a config edit — and the catalog will churn regularly.
2. **Provider abstraction layer.** Don't couple the codebase to OpenRouter's response format. If part of the model roster eventually moves to direct vendor contracts, you need an internal unified interface (`ModelProvider` with `Stream()`, `Classify()`, etc.) behind which OpenRouter, direct Anthropic/OpenAI APIs, or anything else can sit. Skipping this turns a hybrid-sourcing migration into a rewrite.
3. **Request-level idempotency and cancellation.** If a user disconnects mid-generation (closes the tab), does the upstream request keep running and burning money? Needs explicit cancellation of the upstream call on client disconnect — `context` cancellation in Go is nearly free if wired in from the start, expensive to retrofit through the whole stack later.
4. **Circuit breaker / per-model health tracking**, not just a per-request timeout fallback. If a specific model at a specific provider starts failing en masse, the router should exclude it for all users temporarily, rather than every individual request re-hitting the same timeout. This state ("which models are healthy right now") should live in memory/Redis, updated from real failures — without it, the first real incident means every user waits out the same timeout individually instead of a silent automatic bypass.
5. **Conversation storage schema built for migration from day one.** The chat history format (JSON fields, message schema version) should allow new fields without breaking changes — e.g. storing reasoning blocks separately from the final answer, or metadata like which model answered a given message (needed for UI like "Thought for N seconds"). A single raw-text column saves time now, costs weeks later when you need to backfill metadata onto old records.
6. **Stateless app layer**, ready for horizontal scaling even with a single server today. If session state lives in process memory instead of Redis/DB, the moment you add a second instance, a user landing on the other instance loses context. Session state and rate-limit counters (5h/weekly windows) belong in Redis from the start, not local process memory.
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
- Go router: base version implemented — feature-based scoring against `weights.json`, hard filters, manual-mode passthrough, auto-mode tier selection. No circuit breaker, no cost logging, no persistence yet (by design, deferred).
- Classifier: not yet wired to a real model call.
- No real OpenRouter integration yet.
- Unit economics not modeled.

## Next steps (execution)

1. Review the base router implementation — edge cases (empty candidate list after filtering, confidence-escalation correctness, `reason` field readability for debugging).
2. Implement the actual classifier call (pick one candidate model — Gemini Flash-Lite or Claude Haiku 4.5 — not both at once).
3. Hand-label a ~20–30 prompt eval set, run it through classifier + router, check for routing mismatches.
4. Wire up real OpenRouter calls only after step 3 looks sane.
5. Replace placeholder pricing/context-window figures in `models.json` with real OpenRouter numbers.
