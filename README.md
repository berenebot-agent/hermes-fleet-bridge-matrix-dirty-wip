# Hermes Matrix Fleet Bridge

A single-container Matrix + Element chat service that wakes existing Hermes
Compose agents, relays text and attachments, transcribes English voice notes
locally, and stops their
Compose projects after verified inactivity. Go controller with mautrix-go E2EE
and embedded SQLite crypto storage; no additional service.

**MVP implemented.** The bundled homeserver, real Hermes image and Element browser
login have been exercised using isolated state and local fake inference. This is
not a claim that your production hostname, reverse proxy or provider credentials
have been configured or tested. See [verification](docs/verification.md).

## User experience

Give the user one HTTPS URL, a username and a password. They log in to the bundled
Element Web and see their private agent rooms: no signup, room invitations,
mentions, tokens or homeserver selection. Complete Element's normal encryption
setup, keep the recovery key, and verify new devices when asked.

The user can also create their own spaces and rooms, invite an agent's bot into
them, and the bot joins, greets the room once, and answers there like any other
member. The greeting names the room, states that room's reply mode, and gives
the command that changes it:

> Happy to join Finance2. Reply mode: only when mentioned — send
> `!require_mention off` here if you would rather I answer every eligible message.
> `!menu` shows the controls.

In a room the user created the agent answers when mentioned by default;
`!require_mention off` in that room makes it answer everything, and
`!require_mention on` restores the mention requirement. A room holding only the
agent and one other human is a direct conversation, so there the agent answers
every message from the start and no mention is needed:

> Happy to join Finance2. It's just the two of us here, so I answer every message —
> send `!require_mention on` here if you would rather I answer only when
> mentioned. `!menu` shows the controls.

That default is decided once, when the agent joins the room: a room that also
holds another agent's bot is not a direct conversation, and someone joining later
does not change the mode. The agent's own provisioned room never needs a mention,
and a space is joined but never answered in. Invitations from other agents' bots
are refused, so agents cannot wake each other, and each bot is named after its
agent so Element's mention picker shows `Finance2`'s agent rather than an internal
`agent_` localpart. See [the functional spec](docs/functional-spec.md) §5 for the
access-model details.

Ordinary messages use the selected conversation in that room. `!new` starts fresh
without deleting the previous conversation; `!resume` offers a paginated list of
bridge-managed conversations, labelled by first-prompt excerpt and last-used time.
Switching requires no queued/running/held work, pending controls or undelivered
non-menu replies in that room. A switch is announced but does not rearrange Matrix
history. New thread prompts/commands receive an explanation instead of being
dispatched; existing thread history, mappings and admitted work are preserved.
Different rooms can run concurrently; turns within a conversation remain FIFO.
Conversations share the agent's tools/filesystem, not a sandbox.

Container lifecycle is visible in the agent's own room: the agent says when it is
starting, when it is ready, and when it is shutting down after the idle timeout or an
operator Fleet stop. These are ordinary replies, so they arrive in order with
everything else and survive a restart; a shutdown that fails is reported once instead
of being retried silently.

**Security:** no E2EE; the server can read messages. The supplied human account is
a Matrix server administrator, as requested. Room membership is the access
boundary: anyone who can post in a room the bot has joined can use that agent
there, and anyone who can invite the bot chooses where it runs. Docker socket
access is effectively host-root access. This is a trusted-host, trusted-user
application, not public SaaS.

## Operator setup

Requires a **Linux amd64 Docker host**, Docker Compose **2.39+** (5.6 was tested), and an
existing HTTPS reverse proxy. All agents must run on that Docker host. Native
Matrix clients can use the same Matrix endpoint; their own setup screens are not
controlled by the bundled Element configuration.

1. **Prepare bridge-managed agents.** Each configured directory must have a normal
   Compose file and an existing `data/` directory bind-mounted at `/opt/data`.
   Place agents directly under this installation's `data/fleet/` directory;
   all writable agent storage must stay inside its own agent directory.
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
   install -d -m 700 data data/fleet data/templates
   stat -c %g /var/run/docker.sock
   ```

   Edit `.env` locally. Set `PUBLIC_URL`, `MATRIX_SERVER_NAME`, a unique strong
   `ADMIN_PASSWORD` (16+ characters), and `DOCKER_GID` from the last
   command. Set `HFB_UID`/`HFB_GID` to the user/group that own the bridge and member
   data directories (defaults 1000:1000). That user must be able to write each
   member's `data/.env`. Do not print or commit populated environment files.

   Example non-secret settings:

   ```dotenv
   PUBLIC_URL=https://chat.example.com
   MATRIX_SERVER_NAME=chat.example.com
   FLEET_DIR=/opt/hermes-matrix/data/fleet
   FLEET_TEMPLATE_DIR=/opt/hermes-matrix/data/templates
   HERMES_OVERRIDES={"iris":{"service":"hermes","port":8642}}
   ```

   `MATRIX_SERVER_NAME` is the durable Matrix identity, not necessarily the website
   address. **Keep it unchanged after bootstrap.** `PUBLIC_URL` can move to another
   HTTPS origin without replacing accounts, rooms, history or agent sessions.
   Changing `ADMIN_PASSWORD` updates the existing administrator through the private
   authenticated API, logs out its devices, verifies the new login and saves it.
   Server-identity and administrator-username changes still fail closed. See
   [URL and password changes](docs/config-changes.md) before editing those settings.

3. **Use the default parent-directory mounts.** `docker-compose.yml` enables
   Fleet management and mounts `data/fleet/` and `data/templates/` at their same
   absolute host paths. Run Compose from the installation directory; defaults
   use `$PWD`. Set absolute `FLEET_DIR` / `FLEET_TEMPLATE_DIR` in `.env` if needed.
   Never add individual agent mounts beneath the parent mounts. Existing
   installations must migrate paths/state while stopped; see [Fleet setup](docs/fleet.md).

   Startup scans immediate agent directories under `FLEET_DIR`; there is no
   manual agent directory list. Keep unrelated files outside that directory.
   Existing `.fleet-owner.json` records preserve managed identities after a clean
   bootstrap; legacy agents without that record use their directory basename.
   The directory basename is the initial room name. Managed
   Fleet renaming changes the display/room names, not that identity or directory. Basenames
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
   Host-network agents get loopback listeners and need distinct API ports; initial
   discovery recovers `API_SERVER_PORT` from each member's `data/.env`, unless overridden.
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
   An empty fleet directory bootstraps the homeserver and Fleet room without agents.

   Open the HTTPS URL and log in. A message to an offline configured agent starts
   **only that Compose project**, waits for authenticated readiness, and dispatches
   one logical Hermes turn. `IDLE_TIMEOUT` defaults to 15m, `START_TIMEOUT` to 120s,
  and `MAX_PARALLEL_RUNS` to 4 per agent. Typing is room-level, not per-thread.

   Status/stop/approval controls remain available during cold startup and slow
   model queries; lifecycle transitions and reply delivery run independently of
   the command scheduler.

### Phone and Web verification

Rooms use **end-to-end encryption**, including text, images, files and voice notes.
Use Element's stock setup flow: enable key storage, set up recovery, and keep the
recovery key in your password manager. Verify new Web/phone sessions from an
existing device or with that key. The bridge no longer suppresses these screens.
See [client setup](docs/client-setup.md) and [the encryption contract](docs/encryption.md).

Encryption ends at the controller's bot device. Hermes, its history and any model
provider still receive plaintext. The bridge host is trusted; the homeserver and
controller currently share that host. E2EE does not protect against its compromise.

## Commands

Use **plain text `!` commands**, which Element does not intercept as slash commands:

- `!menu`, `!help` — fresh controls with bot-seeded, tappable reaction choices.
  Navigate to status, models, conversations, context/usage, reply mode, stop and
  command help. No widgets, client forks, composer type-ahead or modal dialogs.
  In a room where **more than one of the fleet's agents has a bot**, commands must
  address the intended one (for example, select **Iris** from Element's mention picker and send `@Iris !menu`, or use `iris: !menu`); a bare command is
  refused with a message naming that agent. `!stop` is the exception and still
  stops every agent's conversation in that room. A room with a single bot — a DM,
  or the agent's own provisioned room — never needs an address.
- **A message addressed to another of the fleet's agents is ignored** by every
  other agent wherever it appears: if the text opens with another bot's handle, or
  Element records a mention of another bot and not this one, that agent answers and
  the rest stay silent — no reply, no nag, and never the other agent's addressing
  handed to a model as a prompt. Ordinary prose that merely names another agent is
  not affected.
- `!status` — queue/run information without waking, plus the agent's live state
  (Hermes version, gateway state, connected platforms, today's counters) when it is
  running.
- `!version` — the agent's Hermes version; says so plainly when the agent is stopped.
- `!models [filter]` — paginated configured model/provider choices, with advertised
  route IDs as a fallback (not a guarantee that every route is configured). Can wake
  the agent. Filter by model or provider text; use arrow reactions to change pages.
- `!model <model> [provider]` — opens a **This chat / Global** scope menu. This chat
  changes only the selected conversation; Global changes this agent's existing
  bridge-managed conversations and future default, across its rooms, including
  retained thread sessions. It does **not** change Hermes's global config or other
  agents. Submitted requests remain immutable. Hermes validates availability when
  a run is admitted; saving a selection is not a provider call.
- `!new`, `!reset` — select a fresh conversation, preserving previous history.
- `!resume [filter]`, `!conversations [filter]` — list/resume this room's bridge
  conversations; filter by first-prompt excerpt. No CLI/Telegram session imports.
- `!context` — what the Hermes API exposes about this conversation's context: the
  provider-reported cumulative counters, and the reason live occupancy and headroom
  are unavailable (Hermes computes them inside the gateway; no supported surface
  exposes them). Can wake the agent. Hermes's native `/context` is not a prompt to
  forward: the bridge does not fake it or scrape internal files.
- `!usage` — verified cumulative session counters, **not** live context-window
  occupancy; can wake the agent.
- `!stop` — request cancellation and cancel queued turns in this conversation;
  other rooms continue. Confirmed stopping never means tool effects were undone.
- `!steer <text>` — inject guidance into this conversation's running turn without
  interrupting it; refuses when no turn is running.
- `!retry` — re-submit this conversation's last recorded prompt as a new turn; can
  wake the agent. Tool effects from the earlier attempt are not undone.
- `!restart` — **administrator only**: restart this agent's Hermes gateway. Hermes
  drains its work and restarts itself, and the container's restart policy brings it
  back; the bridge waits for the new gateway and reports whether it is ready again.
  Conversations, history and pending work survive. It restarts the gateway process,
  not the container, so a changed bridge-managed `data/.env` is not re-applied by it.
- `!approve <request-id>` / `!deny <request-id>` — answer this conversation's
  current, exact tool approval. New main-room approvals also automatically offer
  Approve once / Deny reactions to the original sender. No blanket/permanent grants.
- `!discard <turn-id>` — acknowledge a held uncertain turn after authenticated
  inactivity is confirmed. Resend a new message explicitly if you want to retry.
- `!resend` — explicitly retry a blocked Matrix reply outbox with its original
  transaction IDs; visible duplicates remain possible after remote cache expiry.
- `!require_mention on|off` — whether this room must mention the agent before it
  answers in an additional room. The menu labels this **Reply mode**: Only when
  mentioned / Every eligible message. The dedicated room is fixed to Always
  responds. A room that holds only the agent and one other human starts in Every
  eligible message mode; every other room starts in Only when mentioned. Policy
  is per agent/room, survives restarts and conversation switches, and never
  bypasses existing sender/bot checks. Commands and valid menu reactions are
  mention-exempt, except that a command in a room shared by several fleet bots
  must address the intended agent (`!stop` stays global). Changing it does not wake
  Hermes. An agent invited into a room says so once, in a greeting that names the
  room and this command; the greeting comes from the durable reply outbox, so it is
  delivered exactly once and never repeats after a restart.

Reaction menus target **Element X on mobile and bundled Element Web on desktop**;
actual Element X tapping remains an acceptance check, not a verified deployment
claim. Native-client login screens are separate from the bundled Web login.
Each menu belongs to its opener and originating room/conversation. Choices are
single-use, persisted before execution, and stale choices are refused. Opening a
new controls menu replaces old navigation menus, not a still-current approval.
Removing a reaction does not undo an action. Duplicate delivery cannot repeat a
completed local choice. Menus/reaction chips use the durable reply outbox and
stable Matrix transaction IDs; `!resend` also retries blocked menu delivery.

Unsupported commands never pretend to succeed or become agent instructions.
Unrelated reactions, edits and unsupported event types do not wake agents.
An authorized menu choice can wake an agent only for a control that needs its API.

The Hermes commands that work in Telegram are **not** automatically available here:
Hermes is the chat platform in Telegram, while the Matrix bridge is a client of the
Hermes HTTP API. Every Telegram command is therefore re-implemented by the bridge
where a supported surface exists, and named as unavailable where it does not.
[command-parity.md](docs/command-parity.md) is the checklist of that mapping, the
surfaces behind it and how to re-check it after a Hermes upgrade.

## Images, files and voice notes

Send an image or file, optionally with a caption, in the room's main chat. Each
upload is one turn; separate uploads/messages are not automatically grouped.
Existing room/mention/bot rules still apply. Encrypted attachments and remote
homeserver media are refused. Images use Hermes's existing local-path vision tool;
document interpretation depends on the agent's installed tools.

Voice notes carrying the Matrix voice-message marker are transcribed locally in
English using **Parakeet TDT 0.6B v2 INT8 + sherpa-onnx + FFmpeg**. The saved
transcript becomes a normal Hermes prompt automatically. No Groq key or cloud
speech service is used. An ordinary audio-file upload remains a file, not a spoken
instruction. Transcripts never execute bridge `!` commands. Speech recognition can
mishear instructions: use typed messages for exact or high-risk requests.

Agents can return images/files using explicit final-reply references to a
turn-specific outgoing folder. The bridge supplies that folder and marker format
in each prompt; it does not scan the agent's filesystem or upload arbitrary paths.
Selected files are snapshotted before entering the durable reply outbox.

Limits: **20 MiB/file, 100 MiB/turn, 1 GiB managed media/agent, five outgoing
files/turn, five minutes/voice note**. Retain terminal-turn files for seven days;
queued/active/held turns and pending file deliveries remain pinned. Lower
homeserver limits still apply. Local speech runs one recording at a time, with
bounded execution time/memory; longer recordings may hit those limits. The engine
loads on demand, so no model process stays resident while idle.

Compose caps the whole service at **3 GiB**, raised from 768 MiB for local speech.
The persistent model cache uses about **631 MiB**, in addition to its image copy.
The image includes the pinned model; on first voice use it is copied and verified
under `data/controller/models/`. Media also uses the explicitly approved member
`data/matrix-media/` folders; back up these with the bridge state. See
[the media contract](docs/media.md) for paths, recovery, retention and checks.

## Recovery and limits

The controller atomically replaces one mode-0600 JSON state file, fsyncs the file
and directory, and permits one writer. Queue receipt and sync-cursor advancement
commit together. Limited Matrix sync timelines are backfilled before advancing;
an unbridgeable gap fails closed instead of silently dropping messages. That
fail-closed rule protects the agent's own provisioned room; in a room the operator
created, a history gap restarts that room's stream instead of wedging every other
room. State written by an earlier version is upgraded in place on start.
The current state version is 6, including pending ciphertext and encrypted reply
retry state. Older controllers refuse version 6 rather than losing crypto work.
Downgrade only with a matching controller/state backup; never edit the version
number to force a rollback.

Each bot also has a private SQLite crypto database and cryptographic secrets under
`data/secrets/crypto/<agent>/`. Back up **all of `data/` consistently while stopped**,
not just the JSON file. Missing crypto keys are not silently regenerated beside
an existing device. Replies retain their ciphertext across retries and restarts.

Room membership is durable and self-healing: an invitation pending at startup is
accepted on the next start, because an incrementing sync only offers an invite
relative to its cursor. A reply for a room the bot has left is dropped instead of
blocking the reply outbox behind an undeliverable head.

The Hermes request payload and idempotency key are persisted before submission.
Known run IDs are polled after restart; missing acknowledgements reuse the same
request/key **only within the advertised durable retention window**, with a safety
margin. Uncertain or interrupted execution holds the session; it is never blindly
rerun. Other sessions can continue. Held work prevents idle shutdown.

Final text and attachment metadata are persisted in a reply outbox before sending. Stable Matrix transaction
IDs reduce reply duplication but **exactly-once visible delivery is not promised**.
Admission retries are bounded to three, visibility failures to eight, reply retries
to eight before `!resend`. Long successful runs continue polling. Startup and
readiness errors are generic to avoid leaking keys or provider response bodies.

Limits: 16 KiB incoming text/root; 100 queued/active turns per agent and 100 served
rooms per agent; 1,000 stored sessions per room; 200 recent/pending menus per agent,
navigation expiry after 24 hours (older closed-menu records may be evicted);
five choices per list page and up to 1,000 catalog entries; a 50-event per-room
sync timeline; 12,000-byte
UTF-8-safe reply chunks; 8 MiB remote JSON responses; 64 MiB controller state;
10,000-event backfill ceiling. Completed payload copies are removed from state;
recent event IDs remain for deduplication. Hitting a durability/storage safety
limit stops the controller rather than acknowledging data it cannot persist.

Default **[Fleet management](docs/fleet.md)** adds a private operator-only room
for start/shutdown/pause/enable, clean-template creation, full copies, saved full
snapshot templates, display/room renaming and exact-name-confirmed permanent deletion.
The agent menu keeps Refresh status, Rename, Copy and Save as template top-level;
More · Lifecycle contains Start, Shutdown, Pause/Enable, Delete and Force delete.
Renaming never moves the agent directory or restarts its containers.
It uses managed parent-directory mounts in the main Compose file; existing
deployments are not silently migrated.
Matrix history is erased when an agent is deleted: its own room is purged from
the homeserver and leaves your room list.
A delete whose agent API is unreachable is held with the reason stated instead
of waiting silently, and `!fleet force-delete <agent>` deletes it without
draining, after its own confirmation.

Without Fleet management, recreate the service after editing membership/mounts. Adding an agent provisions
its room; removing one disables new dispatch, cancels queued turns, drains admitted
runs, and keeps rooms/history/mappings. The bridge does not automatically retire
the removed project's containers. Removing every member does not wipe Matrix.

## Backup, upgrades and logs

Stop the bundled service before copying **all of `data/` together**, and coordinate
member data backups if you need a consistent agent/bridge restore. Never delete
controller state or secrets to fix startup. Missing/corrupt state beside an
existing database is rejected to prevent historical command replay. Keep the same
hostname and configured absolute member paths when moving hosts.

The Debian-based image pins Tuwunel, Element, Go, Docker CLI, sherpa-onnx and model
artifacts. FFmpeg and its dependencies come from Debian's signed Bookworm repositories.
Back up before an
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
