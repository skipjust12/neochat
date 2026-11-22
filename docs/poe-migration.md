# Poe API

All production AI calls use `https://api.poe.com/v1` with a Poe API key:
chat and streaming, tool calls, classifier, moderation, automatic summaries,
manual compaction, titles, web-search helpers, images and videos. NeoChat's
own API, database, desktop/MCP tools, SearXNG and webpage fetcher retain their
existing interfaces. There is no runtime fallback to Polza.

## Credentials and upgrade

Get a key at <https://poe.com/api_key>. After upgrading, save it in Settings →
Account. Poe chat/media keys and the model-list cache use new browser storage
names; an old Polza key is never automatically reused. Media uses the chat
key unless a separate Poe media key is saved. Keys are not synced or saved in
the server database. NeoChat authentication keys (`nc_...`) remain separate.

`POE_API_KEY` replaces `POLZA_API_KEY` as an optional server fallback. Manual
mode still requires a user key, as before. Remove obsolete vendor-prefixed
model overrides from `.env`, or replace them with Poe bot names. Defaults:

| Call | Poe bot |
| --- | --- |
| Classifier / automatic summarizer | `gemini-3.5-flash-lite` |
| Moderation | `gpt-oss-120b` |
| Title / manual compaction / search helper | `gpt-5.4-nano` |

The same release deployment script can be used. It preserves `.env`, database
volumes and conversation history. A new key must be entered in the browser
after deployment; this upgrade does not turn a Polza key into a Poe key.

## Bots and catalog

`configs/models.json` supplies the existing curated identities, routing scores
and reference estimates. `configs/poe-models.json` is a checked-in snapshot of
Poe's public metadata. Startup merges the two before any calls are served,
so a catalog outage does not restore the previous provider's model slugs.

The live public `GET https://api.poe.com/v1/models` refreshes at most hourly;
forced refresh has a 30-second cooldown. Concurrent requests share one fetch.
Older listed versions move to Legacy when a newer model continues their line.
Only public metadata is shared; user keys, messages and generated responses
are not part of this cache. A failed refresh keeps the previous catalog.

A curated model omitted from Poe's list remains selectable in Manual mode,
with **Unstable** below its name. It is called directly by its bot name (for
example `gpt-6-luna`) through Chat Completions. Its original display name is
preserved. Absence from `/v1/models` does not establish unavailability.
Unlisted bots are excluded from automatic routing. Their tools, attachments,
reasoning controls, pricing and availability have not been confirmed by the
metadata. A refusal is shown to the user; no other bot is silently substituted.
If Poe later lists the bot, its metadata replaces this provisional entry and
the Unstable label disappears.

## Transport and capabilities

- Chat Completions: text, SSE, function calls, image data URLs and file parts
  for documents/videos. Custom bot parameters follow OpenAI SDK `extra_body`
  serialization (flattened into the HTTP body). Parameter names and allowed
  effort values come from Poe's model list. No Polza `plugins` or `video_url`
  extension is sent.
- Responses: models advertising this endpoint alone, and Poe built-in web
  search (`web_search_preview`). Reasoning items, encrypted content, function
  calls/results and source annotations are preserved. Full history is sent
  with `store:false`; server-side continuation IDs are not shared across users.
- Native Messages: only models advertising `/v1/messages`. Claude thinking,
  signatures, tool-use/result blocks and explicit prompt-cache breakpoints
  are preserved. Requests include `anthropic-version: 2023-06-01`. Attachments
  unsupported by the native format use the compatibility transport instead.
- Media: non-streaming compatibility calls download the returned attachments
  and keep them as chat files. Models advertising `/v1/videos` use create,
  poll and authenticated content download. The key never accompanies a CDN
  download; downloads still reject private addresses and oversized content.
  Video references longer than ten seconds are trimmed locally with ffmpeg.
  Choices follow the bot's advertised parameters; duration is Auto when the
  bot exposes no duration setting. Existing inline `show_image`, file sharing,
  regeneration, intermediate text and activity animations are retained.

A broken/truncated SSE stream is an error, not a successful empty answer.
Cancellation closes the upstream request. Retries remain limited to 429;
5xx/timeouts are not automatically replayed. Poe 404 does not trigger the
legacy retry-without-tools path.

## Cache and accounting

Poe's Chat Completions documentation says token-detail objects are empty.
These calls therefore report **no measured cache hits**, even if the provider
internally caches the prompt. A zero counter is not evidence of a cache miss.
Native Claude cache reads/writes and Responses cache reads are recorded only
when returned by the API. Native Claude's ordinary input, cache-read and
cache-write token counts are combined once, without double-counting.

Known cache prices come from Poe metadata (USD per token, converted to USD
per million). Missing cache prices do not imply free tokens or an invented
write premium. The application retains conservative reference estimates
when a bot has no published rate. Unlisted bots retain the curated estimates.

NeoChat's existing `*_cost_usd`, plan reservations and cost logs remain
**internal estimates**, not receipts for Poe point charges. Media reservation
estimates are $0.50/image and $5/video; `pricing.image` is not assumed to be a
flat per-image price. Actual charges can differ, especially for media or
unlisted bots. Set spending limits on the Poe key when needed.

Settings → Usage uses `GET https://api.poe.com/usage/current_balance` and
shows `current_point_balance` in **points**. It is never converted to rubles.
Old RUB alert thresholds are not reinterpreted as points.

## Validation

Without a real key, run:

```bash
go test ./...
node scripts/frontend-state-timing.test.cjs
node scripts/frontend-inline-images.test.cjs
# Install the existing browser-test dependencies first if needed:
node scripts/frontend-poe.browser.cjs
```

Poe-specific tests use local HTTP fixtures to check endpoint/body/auth,
concurrent key isolation, tools, native thinking replay, cache usage/pricing,
SSE termination, catalog TTL, direct Unstable bot calls, media inputs,
polling/downloads and points balance. They do not verify live availability,
provider-side cache creation or real point charges.

For a manual live smoke test, save your Poe key in the UI and try text,
a terminal script with an inline PNG (`show_image`), a tool continuation,
a PDF, a Gemini video attachment, web search, title generation, compaction,
an image edit and video generation. Repeat a sufficiently long Claude
conversation, then inspect `provider: prompt cache` logs for **reported**
cache counters. Short prompts may not reach the model's cache minimum.
Compare charges in your Poe account; only that live check verifies billing.

Official references:

- <https://creator.poe.com/docs/external-applications/openai-compatible-api>
- <https://creator.poe.com/api-reference/createChatCompletion>
- <https://creator.poe.com/api-reference/createResponse>
- <https://creator.poe.com/api-reference/createMessage>
- <https://creator.poe.com/api-reference/listModels>
- <https://creator.poe.com/api-reference/getCurrentBalance>
- <https://creator.poe.com/api-reference/createVideo>

## Exact bot handles and user-owned prompt bots

The picker uses readable labels (Gemini 4 Hydrogen, GPT-6.1 Sol Pro), not raw
Poe slugs or bot descriptions. API identifiers remain separate. Unlisted bots
are sent by their canonical handles, for example `Claude-Opus-5.5`. A correct
handle is necessary but does not establish API availability for an account.

If you create your own public prompt bot, explicitly map the original model
to its handle in `.env`, for example:

```dotenv
POE_BOT_ALIASES={"claude-opus-5.5":"YourPublicOpusBot"}
```

`configs/poe-bot-fallbacks.json` configures `neobotclaude55` as the project
owner’s explicit fallback for `claude-opus-5.5`. The official bot is attempted
first. Only an HTTP 404 before generation starts permits the fallback.
`POE_BOT_FALLBACKS` overrides the file; `{}` disables fallback.
`POE_BOT_ALIASES` is separate and forces an explicit bot mapping when set.

Only use a bot you selected and verified. This setting does not create bots,
change their underlying models, or prove that Poe exposes a given base model
through its API. No community bot is substituted automatically.

To test the documented transports independently of NeoChat, run:

```bash
docker exec -it neochat-server python3 /app/diagnose-poe-bots.py
```

Enter the key locally at the hidden prompt. This makes small paid test
requests via Chat Completions, Responses, Claude Messages where applicable,
and the native Bot Query endpoint used by the official `fastapi_poe` client.
The key is not printed. A native Bot Query success is diagnostic evidence;
NeoChat does not silently switch to this different protocol, which has
different tool, upload and usage accounting semantics.

References:
- <https://creator.poe.com/docs/external-applications/external-application-guide>
- <https://creator.poe.com/docs/server-bots/accessing-other-bots-on-poe>
- <https://github.com/poe-platform/fastapi_poe/blob/main/src/fastapi_poe/client.py>
