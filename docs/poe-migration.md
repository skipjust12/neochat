# Poe Bot Query

Production generations now use the native Bot Query endpoint:
`POST https://api.poe.com/bot/{Exact-Bot-Handle}`, protocol `1.2`, with
`Authorization: Bearer <Poe API key>`. This follows the official
[fastapi_poe client](https://github.com/poe-platform/fastapi_poe/blob/main/src/fastapi_poe/client.py)
and [protocol types](https://github.com/poe-platform/fastapi_poe/blob/main/src/fastapi_poe/types.py).
Chat, classifier, moderation, summaries, compaction, titles, search helpers,
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
