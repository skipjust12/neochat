# Security changes and deployment

The seven findings in the September 2026 audit are addressed in this source tree.

## Request accounting

- Manual Instant requests are charged to the Instant pool, including errors/cancellation; the routing label remains `manual` for API compatibility.
- Every generation, classification, moderation and summary prepays an upper estimate through one atomic Redis operation. Reservations include existing hourly spending buckets, so the upgrade does not reset budgets.
- The estimate uses UTF-8 byte length plus framing headroom and an enforced output ceiling of at most 16,000 tokens (or the model's lower ceiling). Configure prices to match the actual model/provider, including the new `SUMMARIZER_COST_INPUT_PER_MTOK` and `SUMMARIZER_COST_OUTPUT_PER_MTOK` variables; these default to classifier rates.
- Successful calls settle to reported usage even if the browser disconnects. Errors with uncertain usage retain their prepaid amount for operator reconciliation. Do not refund an uncertain call merely because the client disconnected. Retained reservations age out with the existing rolling spend window.
- Only explicit 429 refusals are retried automatically. Ambiguous 5xx errors can represent already-billed work and are not retried.
- Invalid modes and unknown manual models are rejected before paid calls. Insufficient budget returns 429; invalid input returns 400.

## Concurrency and retries

One active request per user is enforced through the shared idempotency store. Chat requests have a 12-minute context deadline; leases last 15 minutes. Completion and release compare an unguessable owner token atomically, so an expired worker cannot modify its replacement. Completed responses remain cached according to `IDEMPOTENCY_TTL` (positive, default 24h).

The idempotency key format changed to avoid ambiguous component boundaries and to separate client keys from request slots. Deploy after draining old workers. Cached replies from the previous key format are not replayed by this version; avoid retrying pre-upgrade submissions during the transition. Existing financial spend keys are unchanged.

## Redis upgrade — preserve existing data

The server now refuses to connect unless Redis uses `maxmemory-policy noeviction`, `appendonly yes`, and `appendfsync always`. Both Compose files specify these settings. Redis errors reject requests instead of disabling rate limiting. Capacity must be monitored: memory exhaustion denies new work rather than discarding financial records.

Following the [Redis persistence upgrade procedure](https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/), for an existing RDB-only Redis instance, enable AOF on the running instance **before restarting it with the new Compose configuration**. First take a backup and stop/drain application traffic. Using an authenticated administrative Redis connection, execute:

```text
CONFIG SET appendonly yes
CONFIG SET appendfsync always
CONFIG SET maxmemory-policy noeviction
INFO persistence
```

Wait for `aof_rewrite_in_progress:0` and `aof_last_bgrewrite_status:ok`, and verify `aof_last_write_status:ok`. Preserve the existing Redis/PostgreSQL volumes and the existing Compose project name. Do not use `down -v`, flush Redis, or start the application against empty replacement volumes. Automatic deletion/recreation of production data is deliberately not part of this change.

## HTTPS deployment

`compose.production.yml` is a **standalone** production file. Do not combine it with the development file using multiple `-f` options: inherited development port mappings are not part of the production configuration.

1. Configure DNS for your hostname, allow inbound 80/443 to the proxy, and set `NEOCHAT_DOMAIN` in the deployment environment (a hostname, not an HTTP URL).
2. Keep the existing `.env` database and vendor credentials. Complete the Redis migration above for an existing installation.
3. Run `docker compose -f compose.production.yml up -d --build`, preserving the previous Compose project name with `-p` if needed.
4. Verify the HTTPS certificate, HTTP-to-HTTPS redirect, health, authentication and Redis persistence before reopening traffic.

Only Caddy publishes ports. PostgreSQL and Redis are on an internal network; the application has no published port. Caddy automatically manages certificates and redirects HTTP to HTTPS. The application requires HTTPS metadata from the explicitly trusted proxy address `172.30.77.2/32`, never from arbitrary forwarded headers. If changing the frontend subnet/address, change `TRUSTED_PROXY_CIDRS` to match. Health checks remain available internally over HTTP.

The development `docker-compose.yml` still binds the application to loopback. It is not a public deployment configuration. The source changes do not install certificates or update a running external server. If credentials were previously used over plain HTTP, rotate those credentials separately.

## Browser and history protection

The frontend has a hash-based CSP, no framing, no referrer disclosure, and private responses are `no-store`. The deployment UI stores the user key only for the browser session and removes its previous localStorage copy. Existing API keys remain valid; key lifecycle management is separate from the seven audit findings.

Conversation history returns the newest 20 messages in chronological order with an optional `next_cursor`. Request older messages with `?before=<next_cursor>`. The UI provides a “Load older messages” button. Database results and JSON responses are bounded; exceptionally large legacy messages are marked `truncated`. Both read endpoints share the per-user rate limit and active-request guard. Queries remain scoped to the authenticated user.

## Validation

Run unit/race tests and integration tests using isolated test databases. The existing migration test drops its schema table, so packages sharing that database must run sequentially:

```text
go test -race -tags=integration -p 1 -count=1 ./...
go vet -tags=integration ./...
go build ./...
govulncheck ./...
```

The CI integration command now uses `-p 1` for this reason. Never run the destructive integration suite against production. `golang.org/x/sys` was updated to v0.44.0 to remove the Windows-only advisory identified in the audit.
