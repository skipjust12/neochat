# Poe Bot Query

Production generations now use the native Bot Query endpoint:
`POST https://api.poe.com/bot/{Exact-Bot-Handle}`, protocol `1.2`, with
`Authorization: Bearer <Poe API key>`. This follows the official
[fastapi_poe client](https://github.com/poe-platform/fastapi_poe/blob/main/src/fastapi_poe/client.py)
and [protocol types](https://github.com/poe-platform/fastapi_poe/blob/main/src/fastapi_poe/types.py).
Chat, summaries, compaction, titles, search helpers,
images and videos use this path. Public model metadata and the points-balance
endpoint remain REST calls; they are not generation requests.

## Transport

- Full history and NeoChat's core prompt are passed as protocol messages;
  assistant messages use role `bot`.
- Tool definitions use the SDK's function schema. The latest tool round is
  returned as top-level `tool_calls` and `tool_results`; earlier rounds are
  retained as readable history. NeoChat continues to execute its own tools.
- Text and JSON tool-call deltas stream into the existing activity/answer UI.
  `replace_response` replaces the streamed answer. Missing `done`, malformed
  events and explicit error events fail the request rather than accepting
  an empty answer. Accepted streams are never automatically replayed.
- Attachments upload through the SDK's endpoint
  `https://www.quora.com/poe_api/file_upload_3RD_PARTY_POST`. Its Authorization
  header is the raw API key, as required by the official uploader. Uploaded
  references are reused within one request's tool rounds, never across users.
- Image/video bots return `file` events or attachment links. Existing safe
  downloads store their bytes with the chat and never forward the key to a CDN.
  Video reference trimming remains local through ffmpeg.
- Custom media controls are sent in the last protocol message's `parameters`.
  Known reasoning parameters come from Poe metadata; unlisted bots without
  advertised controls do not acquire guaranteed effort support automatically.
  Native Bot Query controls, including output caps, are bot-dependent.

The previous Chat/Responses/Messages adapters remain for regression fixtures;
production selects Bot Query by default, including the media client.
`POE_TRANSPORT=compatibility` restores the previous adapters for both; restart
the server after changing the environment. No
compatibility-endpoint guessing or native SDK installation is needed.

## Usage, cache and keys

Native Bot Query text/file events do not provide the compatibility APIs'
measured token/cache usage. `UsageEstimated` explicitly marks local reservation
estimates. Cache reads/writes are left at zero (unknown), never fabricated.
USD plan accounting is approximate and is not a receipt for Poe point charges.
Poe's actual balance is read separately in points. This migration cannot
promise the old native Claude cache-control/signature behavior through a
protocol that does not expose equivalent telemetry.

Poe keys still stay in the browser or request context; optional `POE_API_KEY`
is a server fallback. Old Polza keys are never reused. NeoChat login keys
remain separate. Retry policy does not replay accepted Bot Query responses,
even if an SSE error event says `allow_retry`.

## Catalog and personal models

Poe public metadata refreshes hourly (manual refresh has a cooldown), with a
checked-in startup snapshot. Models absent from that list remain callable by
exact bot handles. Absence from the list does not establish API availability.
`POE_BOT_ALIASES` can force an owner-selected bot. The existing explicit
`claude-opus-5.5` → `neobotclaude55` fallback is retained for initial HTTP 404;
`POE_BOT_FALLBACKS={}` disables it. No other model is substituted silently.

Settings → General → Add a model accepts provider, Poe URL or exact handle,
optional display name, and maximum context in tokens. Empty context uses
catalog metadata, or 128,000 for an unlisted bot. Re-adding a bot updates it.
The value does not increase the upstream model's actual capacity.
Personal models and Legacy overrides persist per account in `user_settings`
and do not enter shared catalog caches or automatic routing.

Search helpers, titles and manual compaction default to `GPT-6-Luna`.
`WEB_SEARCH_MODEL` / `TITLE_MODEL` overrides remain supported. Search uses
SearXNG when configured, otherwise a bot with Poe's web-search parameter.

## Validation limits

Local fixtures test native endpoint/auth, SSE, tools and continuations,
uploads, response replacement, media downloads, custom context persistence,
and private model isolation. These do not establish live access to GPT-6.1
Sol/Fable or actual charges for an account. Live confirmation requires the
user's key, which was not provided to this environment.

References:
- https://creator.poe.com/docs/server-bots/accessing-other-bots-on-poe
- https://creator.poe.com/api-reference/client-sdks
- https://github.com/poe-platform/fastapi_poe

## Observed API access refusal

Live deployment logs showed `This bot does not support API access` for both
Claude-Fable-5.2 and GPT-6-Luna through native Bot Query. Thus exact names and
switching transports do not establish availability. This policy refusal is
shown explicitly and refunds NeoChat's internal reservation; it does not claim
to refund or change charges at Poe. An explicitly configured owner-selected
fallback may run only if the refusal precedes all generated content/tool calls.
The existing public Opus prompt bot is the confirmed working deployment path;
other public prompt bots must be created and tested separately.

The owner-selected Luna fallback is `neobottestapiGPT6Lun`. It also serves
chat titles, search helpers and manual compaction when official GPT-6-Luna
refuses API access. The bot must be public. An explicit `POE_BOT_FALLBACKS`
environment value replaces the checked-in map; remove an obsolete override
or include the Luna mapping there to use it. Existing explicitly configured
`TITLE_MODEL` values still take precedence over the default Luna model.

## Model library window

Settings → General → Manage models opens an independent, responsive dialog.
Account overrides cover display name, subtitle, provider grouping, Poe URL,
tier and context limit, plus Legacy and hidden states. Changes to catalog
entries remain private; the shared catalog is never rewritten. Manual requests
resolve the account's selected entry with these overrides before validation
and generation. Auto routing keeps the original shared catalog.

The order persists in `model_order`; new catalog models append without losing
existing choices. Both drag and up/down buttons are available. Duplicate
review compares exact normalized Poe links and normalized display names,
shows each link, and asks which entry to keep. Name-only matches can be different
bots and are never deleted automatically. Cleanup removes personal definitions
and hides catalog copies; existing chat history is retained.

Authenticated model actions:
- `PATCH /account/models/{id}` updates fields (or `{ "reset": true }`).
- `PUT /account/models/order` accepts `{ "ids": [...] }`.
- `POST /account/models/actions` accepts `{ "action": "remove", "ids": [...] }`.

Model settings have a combined 256 KB bound, with up to 200 personal models.
Ordinary preferences updates retain their 32 KB input bound and preserve all
server-managed model settings. The model picker restores a saved personal ID
even if settings arrive before the personal catalog, fixing the lost selection
on reload/sign-in. Personal edits and ordering never enter the shared browser
catalog cache.

## Conversation fidelity audit

Native Bot Query retains earlier tool rounds using the documented
`ProtocolMessage` format: `role=bot, message_type=function_call` with a JSON
array of calls, followed by `role=tool` containing a JSON array of results.
The latest round still uses the SDK's top-level `tool_calls`/`tool_results`.
Intermediate assistant prose remains in query, separately from function calls.
Tool results are not rewritten into user messages. Text parts are all retained,
and request parameters stay on the actual user message during continuations.

Requested-model reasoning metadata is bound before an explicit alias or
owner-selected fallback changes the bot handle. Each new helper request resets
that binding. This preserves known supported controls; it does not establish
reasoning support for a bot that exposes no parameter metadata.

Each native request logs the bot handle, input/protocol message counts,
text character count, requested effort and parameter name. `effort=unsupported:…`
means the requested control could not be mapped from the available metadata.
Message bodies, keys and attachments are not logged. Payload regression tests
cover long histories, multiple tool rounds and alias/fallback effort forwarding;
these are transport checks, not a live model-quality benchmark.
