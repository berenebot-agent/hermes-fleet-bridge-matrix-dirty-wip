# Hermes Matrix Fleet Bridge

A single-container Matrix + Element chat service that wakes existing Hermes
Compose agents, relays text through their authenticated run API, and stops their
Compose projects after verified inactivity. Go controller, standard library only.

**MVP implemented.** The bundled homeserver, real Hermes image and Element browser
login have been exercised using isolated state and local fake inference. This is
not a claim that your production hostname, reverse proxy or provider credentials
have been configured or tested. See [verification](docs/verification.md).

## User experience

Give the user one HTTPS URL, a username and a password. They log in to the bundled
Element Web and see their private agent rooms: no signup, room invitations,
mentions, tokens, homeserver selection or required device verification.

The user can also create their own spaces and rooms, invite an agent's bot into
them, and the bot joins and answers there like any other member. In a room the
user created the agent answers when mentioned by default; `!require_mention off`
in that room makes it answer everything, and `!require_mention on` restores the
mention requirement. The agent's own provisioned room never needs a mention, and
a space is joined but never answered in. Invitations from other agents' bots are
refused, so agents cannot wake each other. See
[the functional spec](docs/functional-spec.md) §5 for the access-model details.

Ordinary room messages share a room conversation. Each Matrix thread has its own
durable Hermes session, initially given the quoted root message, not a copy of the
room's entire history. Sessions are keyed per room, so the same thread in two
rooms is two conversations. Different sessions can run concurrently; turns within
a session remain FIFO. Sessions share the agent's tools/filesystem, not a sandbox.

**Security:** no E2EE; the server can read messages. The supplied human account is
a Matrix server administrator, as requested. Room membership is the access
boundary: anyone who can post in a room the bot has joined can use that agent
there, and anyone who can invite the bot chooses where it runs. Docker socket
access is effectively host-root access. This is a trusted-host, trusted-user
application, not public SaaS.

## Operator setup

Requires a **Linux Docker host**, Docker Compose **2.39+** (5.6 was tested), and an
existing HTTPS reverse proxy. All agents must run on that Docker host. Native
Matrix clients can use the same Matrix endpoint; their own setup screens are not
controlled by the bundled Element configuration.

1. **Prepare bridge-managed agents.** Each configured directory must have a normal
   Compose file and an existing `data/` directory bind-mounted at `/opt/data`.
   Stop agents before initial enrollment. Their images must already be present;
   the controller never builds or pulls agent images. Use a current Hermes image
   exposing `/v1/runs`, authenticated `/health/detailed`, and durable run
   idempotency. The tested digest is recorded in `docs/verification.md`.
   Disable native Telegram/Matrix adapters, cron and other unmanaged activity on
   these agents: the bridge's lifecycle guarantee covers its own work only.

2. **Create private configuration and storage** from this repository's `repo/`
   directory:

   ```bash
   cp .env.example .env
   chmod 600 .env
   install -d -m 700 data
   stat -c %g /var/run/docker.sock
   ```

   Edit `.env` locally. Set `PUBLIC_URL`, `MATRIX_SERVER_NAME`, a unique strong
   `ADMIN_PASSWORD` (16+ characters), `HERMES_DIRS`, and `DOCKER_GID` from the last
   command. Set `HFB_UID`/`HFB_GID` to the user/group that own the bridge and member
   data directories (defaults 1000:1000). That user must be able to write each
   member's `data/.env`. Do not print or commit populated environment files.

   Example non-secret settings:

   ```dotenv
   PUBLIC_URL=https://chat.example.com
   MATRIX_SERVER_NAME=chat.example.com
   HERMES_DIRS=/srv/hermes/iris,/srv/hermes/lyra
   HERMES_OVERRIDES={"iris":{"service":"hermes","port":8642}}
   ```

   `MATRIX_SERVER_NAME` is Compose/environment-defined, not hardcoded. In this
   single-origin MVP it must match the public URL's hostname. **Choose the final
   hostname before bootstrap.** Identity, URL and administrator-name changes
   against existing data fail explicitly; changing them is a migration, not a
   reset. Updating `ADMIN_PASSWORD` never silently resets an existing password.

3. **Add each same-path member bind mount** to `docker-compose.yml`:

   ```yaml
   volumes:
     - ./data:/data
     - /var/run/docker.sock:/var/run/docker.sock
     - /srv/hermes/iris:/srv/hermes/iris
     - /srv/hermes/lyra:/srv/hermes/lyra
   ```

   The directory basename is the stable agent identity and room name. Basenames
   must be unique lowercase localparts (`a-z`, digits, `_`, `.`, `-`); duplicate
   Compose project names are rejected. Use `HERMES_OVERRIDES` only when needed:
   `service`, `project`, `network` (Docker network name), and `port`.

   The controller preserves unrelated `data/.env` values and existing strong,
   unique API keys. It manages `API_SERVER_ENABLED`, `API_SERVER_HOST`,
   `API_SERVER_PORT` and a generated key if absent. **Remove explicit conflicting
   `API_SERVER_*` service environment entries**; Compose would override the file.
   An `env_file: ./data/.env` is supported. API key rotation is not automated.

   Bridge-network agents get an internal `0.0.0.0` API listener reached through
   their inspected private container IP, never through a published API port.
   Host-network agents get loopback listeners and need distinct `port` overrides.
   Do not expose Hermes API ports publicly. Agent state is never replaced and
   `docker compose down` is never passed volume/image deletion flags.

4. **Configure Nginx Proxy Manager.** Forward the chosen HTTPS hostname to the
   Docker host's frontend port (default `8088`, HTTP backend). Use a valid TLS
   certificate and a proxy timeout of at least 120 seconds for Matrix long polls.
   Proxy the whole origin, including `/.well-known/matrix/client`, static assets
   and `/_matrix/client/*`. Do not add a separate homeserver proxy or federation
   port. Restrict the backend port by host firewall to the trusted proxy/network;
   never expose the Docker socket. `MATRIX_LISTEN_ADDR` stays loopback-only.

5. **Build and start**:

   ```bash
   docker compose down && docker compose up -d --build
   docker compose logs --tail=200
   ```

   This creates accounts, closed-registration homeserver configuration, bot
   identities and joined private rooms automatically. The frontend reports 503
   during initialization. `GET /healthz` reports readiness, held turns, blocked
   replies and sync failures without exposing credentials. Docker health becomes
   unhealthy when operator attention is needed; it does not kill active agents.
   An empty `HERMES_DIRS` permits a Matrix/Element-only bootstrap.

   Open the HTTPS URL and log in. A message to an offline configured agent starts
   **only that Compose project**, waits for authenticated readiness, and dispatches
   one logical Hermes turn. `IDLE_TIMEOUT` defaults to 15m, `START_TIMEOUT` to 120s,
  and `MAX_PARALLEL_RUNS` to 4 per agent. Typing is room-level, not per-thread.

   Status/stop/approval controls remain available during cold startup and slow
   model queries; lifecycle transitions and reply delivery run independently of
   the command scheduler.

## Commands

Use **plain text `!` commands**, which Element does not intercept as slash commands:

- `!help`, `!status` — supported commands and queue/run information, without waking.
- `!models` — configured model inventory, with advertised route IDs as a fallback.
- `!model <model> [provider]` — select model/provider for not-yet-started turns in
  this conversation. Already-submitted requests are immutable. Hermes validates
  availability when a run is admitted; saving a selection is not a provider call.
- `!context` — verified cumulative session usage, **not** live context-window
  occupancy. Model listing and context inspection can wake the agent.
- `!stop` — request cancellation and cancel queued turns in this conversation;
  other threads continue. Confirmed stopping never means tool effects were undone.
- `!approve <request-id>` / `!deny <request-id>` — answer this conversation's
  current, exact tool approval. Approve-once only; no blanket/permanent grants.
- `!discard <turn-id>` — acknowledge a held uncertain turn after authenticated
  inactivity is confirmed. Resend a new message explicitly if you want to retry.
- `!resend` — explicitly retry a blocked Matrix reply outbox with its original
  transaction IDs; visible duplicates remain possible after remote cache expiry.
- `!require_mention on|off` — whether this room must mention the agent before it
  answers. Per room, any member can set it, and it survives restarts. Commands
  always work without a mention, so this one is reachable while it is on.

Unsupported commands never pretend to succeed or become agent instructions.
Edits, reactions, attachments and non-text events do not wake agents.

## Recovery and limits

The controller atomically replaces one mode-0600 JSON state file, fsyncs the file
and directory, and permits one writer. Queue receipt and sync-cursor advancement
commit together. Limited Matrix sync timelines are backfilled before advancing;
an unbridgeable gap fails closed instead of silently dropping messages. That
fail-closed rule protects the agent's own provisioned room; in a room the operator
created, a history gap restarts that room's stream instead of wedging every other
room. State written by an earlier version is upgraded in place on start.

Room membership is durable and self-healing: an invitation pending at startup is
accepted on the next start, because an incrementing sync only offers an invite
relative to its cursor. A reply for a room the bot has left is dropped instead of
blocking the reply outbox behind an undeliverable head.

The Hermes request payload and idempotency key are persisted before submission.
Known run IDs are polled after restart; missing acknowledgements reuse the same
request/key **only within the advertised durable retention window**, with a safety
margin. Uncertain or interrupted execution holds the session; it is never blindly
rerun. Other sessions can continue. Held work prevents idle shutdown.

Final text is persisted in a reply outbox before sending. Stable Matrix transaction
IDs reduce reply duplication but **exactly-once visible delivery is not promised**.
Admission retries are bounded to three, visibility failures to eight, reply retries
to eight before `!resend`. Long successful runs continue polling. Startup and
readiness errors are generic to avoid leaking keys or provider response bodies.

Limits: 16 KiB incoming text/root; 100 queued/active turns per agent and 100 served
rooms per agent; a 50-message per-room sync timeline; 12,000-byte
UTF-8-safe reply chunks; 8 MiB remote JSON responses; 64 MiB controller state;
10,000-event backfill ceiling. Completed payload copies are removed from state;
recent event IDs remain for deduplication. Hitting a durability/storage safety
limit stops the controller rather than acknowledging data it cannot persist.

Recreate the service after editing membership/mounts. Adding an agent provisions
its room; removing one disables new dispatch, cancels queued turns, drains admitted
runs, and keeps rooms/history/mappings. The bridge does not automatically retire
the removed project's containers. Removing every member does not wipe Matrix.

## Backup, upgrades and logs

Stop the bundled service before copying **all of `data/` together**, and coordinate
member data backups if you need a consistent agent/bridge restore. Never delete
controller state or secrets to fix startup. Missing/corrupt state beside an
existing database is rejected to prevent historical command replay. Keep the same
hostname and configured absolute member paths when moving hosts.

The image pins Tuwunel, Element, Go and Docker CLI artifacts. Back up before an
upgrade: RocksDB upgrades may not be reversible. Compose allows 30 minutes for
homeserver migrations to shut down safely. The controller supervises Tuwunel;
either component's fatal failure stops the service. Homeserver stdout/stderr is
not forwarded because bootstrap/error output may contain credentials. Never use
debug logging or dump Docker inspection/environment data into shared logs.

```bash
docker compose logs -f
```

Third-party notices/licences are in the image under `/opt/element/licenses/` and
served at `/licenses/`. See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## Development and verification

Go is Docker-only; no host installation or third-party Go modules:

```bash
./hermes-fleet-bridge-matrix-go.sh gofmt -l .
./hermes-fleet-bridge-matrix-go.sh vet ./...
./hermes-fleet-bridge-matrix-go.sh test -race ./...
```

Tests use fake local endpoints. The separately authorized real-image checks use
scratch state/projects and local fake inference, never live fleet directories:

```bash
docker buildx build --load --provenance=false -t hermes-fleet-bridge-matrix:local .
python3 tests/integration.py --keep
PLAYWRIGHT_MODULE=/path/to/already-installed/playwright node tests/browser-check.cjs
```

Browser checks require an **already installed** Playwright/Chromium, not a runtime
dependency. They proxy the test HTTPS origin inside the browser, so do not verify
production DNS/TLS/NPM. `--keep` deliberately retains the generated scratch bridge
for the browser check; its exact container name/root are recorded privately in
`tests/logs/browser-fixture.json`. Stop/remove that named bridge and run Compose
`down` on the recorded scratch member files afterwards. Without `--keep`, the
integration script removes its test containers automatically. Test artifacts and
credentials stay under gitignored `tests/logs/`.

This repository remains entirely separate from the Telegram fleet bridge.
