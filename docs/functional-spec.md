# Hermes Matrix Fleet Bridge — Functional Specification

**Version:** 0.4 (implemented media extension)

**Encryption amendment (2026-10-21):** The operator approved replacing unencrypted
V1 transport with Matrix E2EE, mautrix-go and embedded SQLite crypto storage, plus
a fresh Matrix deployment without preserving old accounts/history.
[encryption.md](encryption.md) is authoritative wherever older sections below
describe unencrypted rooms, stdlib-only transport or JSON-only persistence.
Human verification and recovery now use stock Element flows with key storage on.
The Web encryption-setup suppression module is removed.

**Date:** 2026-10-19
**Project:** `hermes-fleet-bridge-matrix-dirty-wip`  
**Reference implementation:** [Telegram Hermes Fleet Bridge](https://github.com/berenebot-agent/hermes-fleet-bridge-dirty-wip)

## 1. Purpose and non-negotiable outcome

Build a **standalone, self-hosted Matrix chat platform for a fleet of existing Hermes agents**. It replaces Telegram as the chat surface, while carrying forward the existing fleet bridge's primary job: automatically start Hermes containers on a message and stop them when idle.

**User experience:** The end user receives **one HTTPS URL, a username, and a password**. Provisioning handles accounts, rooms and membership. They must **not** need to register, create rooms, accept invitations, change permissions, find bot IDs, type `@mentions`, install plugins or manage access tokens. Bundled Web fixes the homeserver; native clients control their own server selection. Complete normal Element account/device verification, enable key storage and save the recovery key. New devices may need verification or recovery. Do not suppress these flows. See [client setup](client-setup.md).

The user is not restricted to what was provisioned: they may create spaces and rooms, invite an agent's bot into them, and the bot joins, greets the room once, and answers there like any other member. In a room the operator created, the bot answers when mentioned by default — unless that room is a direct conversation with one other human, where it answers every message — and can be told otherwise with `!require_mention off`; its own provisioned room never needs a mention. See §5.

The *operator* performs one-time deployment by supplying the hostname, account credentials, agent Compose directories, and Docker access. **Everything relating to Matrix configuration, accounts, bots, rooms, permissions, and Element setup happens automatically.**

Keep this project entirely separate from the existing Telegram bridge. Do not alter the Telegram bridge repository.

## 2. Architecture

One Docker image / one Docker Compose service bundles:

| Component | Proposed implementation | Function |
| --- | --- | --- |
| Matrix homeserver | Tuwunel 1.9.2, digest pinned | Accounts, private rooms, history, sync, threads |
| Web chat client | Element Web 1.12.13 static assets, digest pinned | Guaranteed bundled browser UI; compatible native Matrix clients may connect |
| Fleet controller | Go | Provision users/rooms, receive Matrix events, durable dispatch to Hermes API, control Compose lifecycle |
| HTTP frontend | Internal Go HTTP server/reverse proxy | One public hostname for Element and `/_matrix/*` routes |
| Local English speech | sherpa-onnx 1.13.8, Parakeet TDT v2 INT8, FFmpeg | Voice notes become durable text prompts; CPU-only, on demand |

- **One external HTTPS URL.** TLS terminates at an existing reverse proxy such as Nginx Proxy Manager.
- **One exposed application service.** Internal homeserver and controller listeners are not published separately.
- **One persistent `./data` bind mount** for Matrix, controller state, and protected secrets.
- Linux host networking: the frontend has one configurable fixed port; the homeserver listener is loopback-only. Agent bridge IPs are inspected locally; host-network members use loopback API ports.
- No external PostgreSQL, Redis, hosted service, separate Element container, or additional manual Matrix administrator is required.
- Existing Hermes agent containers and persistent directories remain outside this application.
- The runtime base is Debian Bookworm, explicitly approved for glibc speech-engine compatibility. The pinned standalone speech artifact currently targets Linux amd64. E2EE adds approved Go modules and embedded SQLite; no additional service, Python or Node runtime is added.

**Message transport decision for initial build:** The always-running bridge controller owns Matrix bot identities and Matrix event consumption. It queues messages and relays them to Hermes through its existing authenticated HTTP run API. **Do not simultaneously enable a Hermes native Matrix client for the same room/bot**, or duplicate consumption/replies may result. This design deliberately avoids depending on a stopped native Matrix adapter to wake and recover the first incoming event.

Version pins and runtime contracts are verified capabilities, not assumptions.
See `docs/verification.md` for exact tested artifacts and the remaining deployment checks.

## 3. Packaging and deployment

Normal installation layout:

```text
/opt/hermes-matrix/
├── docker-compose.yml
├── .env
└── data/
    ├── matrix/
    ├── controller/
    ├── secrets/
    ├── fleet/       # one self-contained Compose directory per managed agent
    └── templates/   # clean and full-snapshot templates
```

Operator configuration (see `.env.example` and `README.md` for all settings):

```dotenv
PUBLIC_URL=https://chat.example.com
MATRIX_SERVER_NAME=chat.example.com
ADMIN_USERNAME=admin
ADMIN_PASSWORD=replace-with-strong-secret
IDLE_TIMEOUT=15m
START_TIMEOUT=120s
```

Initial agent enrolment uses a comma-separated list of existing **Docker Compose project directories** under `./data/fleet`. Fleet-created members are also persisted in controller state. The main Compose file mounts the fleet and template parents at their *same absolute host paths* so relative member bind mounts resolve correctly; do not mount individual agents separately. It also mounts the Docker socket and durable `./data`. `FLEET_DIR` / `FLEET_TEMPLATE_DIR` can override the default `$PWD/data/fleet` / `$PWD/data/templates` paths.

Allow optional overrides for display name, Compose project/container, network selection and Hermes API port when inference fails. Do not require such overrides in the normal case.

On first start validate public hostname, administrator credentials, mounted directories, Docker/Compose access, and secure storage permissions. Reject invalid configuration explicitly. `MATRIX_SERVER_NAME` is the durable Compose/.env-defined Matrix identity; it need not match the `PUBLIC_URL` hostname. Persist the identity, origin and administrator name. Refuse server-identity/administrator-name changes after bootstrap, but reconcile valid HTTPS public-origin changes without replacing data. An explicit `ADMIN_PASSWORD` change updates the existing administrator through the private authenticated password-reset API, revokes its devices and verifies the desired login before saving the new credential. Restart recovery first tries the desired login to recognize a remotely applied change without repeating it. See [config-changes.md](config-changes.md).

## 4. Completely automated bootstrap

When `./data` is empty, the entrypoint/controller must:

1. Validate required inputs and permissions.
2. Generate and persist stable Matrix homeserver configuration, signing keys, registration/admin bootstrap secrets, and controller credentials.
3. Start the bundled homeserver with registration and federation already disabled. Use its supported shared-secret registration interface on the private loopback listener; never route the admin endpoint through the public frontend.
4. Create the initial human administrator account from deployment settings.
5. Keep public registration and federation disabled; restrict the homeserver to approved users.
6. Generate Element configuration pointing at the bundled homeserver, removing homeserver selection / account-creation friction where the chosen client allows.
7. Discover and validate configured Hermes Compose projects.
8. Provision a Matrix bot account and private room for each agent. The bot creates a private room, invites the administrator, grants the administrator room power, is named after its agent (so Element's member list and mention picker are usable), and the controller performs the administrator's join with its authenticated client token. No manual invitation acceptance. Bot name and power levels are repaired idempotently on reconciliation, because a `private_chat` invitee otherwise holds power level 0 and cannot rename the room or file it into a space.
9. Accept room and space invitations the bot receives from an approved human, including invitations that were already pending at startup. Never accept an invitation from another agent's bot. When a room is joined by invitation, post one greeting that names the room and states its reply mode with the command that changes it.
10. Save stable mappings, room IDs, event offsets, credentials and queue state with safe filesystem permissions.
11. Mark the application ready only when the user can actually log in and see the agent rooms.

On every subsequent startup, reconcile missing accounts, rooms and invitations **idempotently**. Do not reset histories, duplicate rooms, rotate working credentials unnecessarily, or overwrite unrelated Hermes configuration. Only an explicit `ADMIN_PASSWORD` change authorizes updating the administrator's password; bot credentials are not rotated. Pending invitations are reconciled once per start, because an incrementing sync only reports an invite relative to its stored cursor.

Persist credentials before creating accounts, use deterministic room aliases and ownership state to recover a creation whose local mapping was not saved, and prefer an existing persisted room ID over alias lookup. A missing/deleted existing room fails closed; its history is never silently replaced. Missing controller state or secrets beside an existing homeserver database requires a consistent restore, not fresh bootstrap.

**Acceptance:** A new human user opens the URL, enters the supplied username/password, and immediately sees working agent rooms. No wizard, room join, `@mention`, bot activation or Matrix administrative steps.

## 5. Agent discovery, accounts and rooms

Fleet management is the only membership mode. Startup scans immediate agent directories under `FLEET_DIR`, not an incidental list of running containers. Existing controller identities remain authoritative; new discovery recovers `.fleet-owner.json` identities or uses directory basenames for legacy agents. Templates are discovered from valid manifests under `FLEET_TEMPLATE_DIR`. There is no manual agent directory list.

**Managed Fleet:** `FLEET_DIR` and `FLEET_TEMPLATE_DIR` are required. Durable controller state owns enrolled members and members created through the private operator-only Fleet room. Its compact agent menu keeps status, display/room renaming, full duplication and full-snapshot template saving top-level; More · Lifecycle groups start, shutdown, pause/enable and guarded permanent deletion. Clean/full templates support creation. New identities are independent of reusable display names. Rename preserves identity, directories, containers and conversations, and durably retries Matrix profile/room updates without draining work. All writable agent storage is self-contained under the mounted managed parent; future connected-only membership must not imply deletion authority. Lifecycle/snapshot operations drain admitted work, persist progress and fail closed across restart; permanent deletion erases the agent's own Matrix room with its messages. Detailed discovery, ownership, confirmations, snapshot semantics, setup and limits are in [fleet.md](fleet.md). Use Fleet controls to remove agents, not filesystem deletion.

Managed Fleet is enabled by the default Compose deployment, with parent mounts
for `./data/fleet` and `./data/templates` at their absolute host paths. It extends
the original V1 boundaries below without adding a service, dependency, public
registration endpoint, or Matrix-history deletion. Existing-directory deployments
must migrate their storage/state deliberately before adopting this default.

For each valid project:

- Create/reuse a bot user, e.g. `@iris:chat.example.com`.
- Create/reuse one **private dedicated room**, e.g. **Iris**.
- Automatically join the administrator and that agent's bot; never require manual invitations.
- Give the administrator power level 100 in that room so the human can administer it and add it to a space.
- Map project directory, Compose project, bot account, room ID, API key, and persistent Hermes session IDs.
- Configure or discover the Hermes API key by supported persistent `.env` conventions; preserve existing keys and unrelated configuration.
- Allow discovery/reconciliation after an operator adds a new directory to the configuration.
- Preserve messages, rooms and membership history after an operator removes an agent by removing its directory. Disable new dispatch, cancel queued turns, drain admitted runs, and leave final container retirement to the operator. Do not automatically run Compose down against a removed project. Fleet's confirmed permanent deletion is a separate, explicitly destructive action that erases the agent's own Matrix room and its messages.
- Do not require mentions in dedicated agent rooms. Prevent bot-to-bot reply loops.

### Rooms and spaces the operator adds

Membership is the access boundary: anyone who can post in a room the bot has joined may address the agent there, and a user who can invite the bot chooses where it serves. This is deliberate, and it means access control rests on who can invite people into the operator's rooms.

- The bot joins any room or space an **approved human** invites it to, including invitations already pending at startup. Invitations from bot accounts (`agent_*`) are never accepted, so agents cannot wake each other.
- Each room the bot serves has its own backfill anchor, mention policy, selected conversation and retained bridge-managed conversation inventory.
- **Mention policy is per agent/room.** The provisioned room never requires a mention and cannot be switched to mention-only mode. A room the operator created that holds only this bot and one other human member is a **direct conversation**: it starts without a mention requirement, and the greeting says the agent answers every message there. Every other room the operator created requires a mention until `!require_mention off` is run in that room. A room that also holds another agent's bot is never a direct conversation, so two agents never answer every message in it. The direct-conversation default is decided once, when the room is first tracked: a member joining later does not change it, and `!require_mention on` restores the mention requirement. On joining a room by invitation the agent posts one greeting naming the room, stating that room's reply mode and how to change it — once per room, from the reply outbox, so a restart or a redelivery cannot repeat it and a room the operator already has gets no retroactive greeting. Commands and valid menu reactions are always processed without a mention, except in a room shared by several of the fleet's agents' bots, where an unaddressed command is refused with a message naming the agent the sender should address (`!stop` stays global and acts in every bot's conversation there). A message addressed to another agent of the fleet — its handle opening the text, or an explicit mention of it without this agent — is ignored by the other agents rather than answered, whatever the room's reply mode; only the opening token counts, so prose that merely names another agent still reaches this one. The menu calls this Reply mode; changing it neither wakes Hermes nor changes conversation selection.
- A **space** is joined but never dispatched from: it is a container, not a conversation.
- Senders from reserved agent namespaces are ignored, as are edits, unrelated reactions and unsupported event types. Supported image/file/audio messages follow the same room/mention rules. Only reactions bound to a persisted menu, exact owner and originating room can select controls. A deleted agent stops dispatching and does not announce itself in operator-created rooms.
- Replies are routed back to the room and thread they came from. A room the bot is removed from stops being served; a later re-invite starts a fresh conversation.
- At most 100 rooms are served per agent; further invitations stay pending.

Additional human users may be supported later; when explicitly provisioned/approved by an operator, they should also receive usable rooms automatically. Do not expose open self-registration.

## 6. Room conversations and retained thread mappings

A Matrix room is the conversation surface. By explicit operator choice, the mobile
interaction follows Telegram-style New/Resume switching, not Matrix threads.

- Plain messages use that room's selected, persisted Hermes session. Existing room-session IDs remain unchanged on upgrade.
- New/Reset selects a fresh session without deleting Matrix or Hermes history. Resume lists only that room's bridge-managed main-room conversations, labelled by short first-prompt excerpt and last-used UTC time; no extra title-generation calls or external-session imports.
- Switch only when that room has no queued/running/held turns, pending controls or undelivered non-menu replies. Announce switches; never rearrange or erase the visible Matrix timeline.
- New thread prompts/commands receive a main-room explanation and do not wake Hermes. Preserve existing thread mappings/history and safely reconcile previously admitted work; legacy replies retain their original routing. Threads are not included in the Resume picker.
- Keep the bridge registry key stable while adopting a valid session ID returned by a completed run. Upstream `/v1/runs` resolves compression-continuation tips when resuming by ID; do not invoke source-ending forks.
- Keep message ordering FIFO per session, with bounded parallel sessions per agent (default four). Session separation is conversation/history isolation, not isolation of shared files/tools.
- Preserve session identities across bridge and Hermes container restarts.
- Support typing indicators while an agent starts and runs, insofar as Matrix clients display them.
- Menu reactions are controller controls, never agent prompt text. Edits and unrelated reactions are not instructions. Attachment captions and voice transcripts are agent inputs, never bridge controls.

## 7. Message handling and lifecycle

**On a new authorized Matrix message:**

1. Receive an event from the always-running controller and validate room membership, event type and thread.
2. **Persist the event ID and queued turn before advancing durable Matrix sync state.**
3. Deduplicate repeat Matrix deliveries and place the turn in the appropriate agent/session FIFO queue.
   For media, validate/download/stage first; transcribe marked voice notes and save
   the transcript before creating any Hermes run. Failed preprocessing does not wake Hermes.
4. Check the agent Compose project and call `docker compose up -d` if necessary.
5. Wait for authenticated Hermes API readiness, subject to a startup timeout.
6. Create the Hermes run with a stable session and idempotency key where available.
7. Relay room-level typing and approval/status notices; send the final text answer and explicitly selected output attachments to the originating room/thread. A text reply is sent as `org.matrix.custom.html` formatted markdown so Element renders bold and code instead of showing literal `**`, with the plain markdown `body` retained as the fallback. Token streaming is not part of this MVP.
8. Persist run and reply progress; acknowledge successful completion logically once.

**Container lifecycle notices:**

- The bridge posts a short notice in the agent's own provisioned room when it brings the
  Compose project up, when authenticated readiness is reached, and when it takes the
  project down (idle timeout or an operator fleet stop). A wake triggered from another
  room still reports in the agent's own room, so container state has one home.
- These are ordinary reply-outbox messages: durable, ordered, retried, and free of
  secrets. A shutdown that fails is reported once; the retry does not repeat the message.

**Idle shutdown:**

- Only start idle timing after the bridge-managed queued/running turns for that agent have completed.
- Do not stop an agent with a known active request, tool call, or approval wait.
- After configured inactivity, re-check active state and stop/down **only the relevant managed Compose project**.
- Retain bind-mounted agent data. Existing Hermes images/state are not changed by this service.
- **V1 agents must be managed exclusively by the bridge.** Cron jobs, scheduled activity, external chat adapters and unmanaged concurrent sessions are outside the safety guarantee. Do not claim to protect workloads not observable through Hermes API.

**Failure/restart behavior:**

- Persist Matrix sync tokens, seen event IDs, queued requests, Hermes run IDs, and reply status in `./data`.
- Restart during receipt, cold wake, Hermes run, or Matrix sending must resume/reconcile safely.
- No duplicated *logical Hermes turn* from Matrix redelivery; enforce idempotency where supported and explicitly handle uncertain outcomes.
- Require advertised durable Hermes idempotency and complete authenticated activity counters before dispatch. Persist the exact request body/key before submission; recover known run IDs without creating new runs. A lost acknowledgement may reuse that key only within the smaller of saved/current advertised retention windows, with a safety margin. Expired, unobservable or interrupted execution holds its session; never automatically replay possible tool actions.
- Resolve held turns with `!discard <turn-id>` only after authenticated inactivity is confirmed. Another attempt is an explicitly resent new message, not an automatic replay. Other sessions remain usable; held work disables idle shutdown.
- Exactly-once visible Matrix replies cannot be promised without supported remote idempotency; document residual duplicate risk.
- Persist a reply outbox before sending; use stable Matrix transaction IDs per chunk across retries/restarts. After eight failed sends, require `!resend`. A remote deduplication-cache expiry may still cause a visible duplicate.
- Bound retries. Report actionable failures within the agent room without leaking credentials.

### 7.1 Media contract

`docs/media.md` defines the supported Matrix event shapes, filesystem and
final-reply attachment protocol. Hermes remains unchanged: files use existing
local-path tools and voice becomes text before the authenticated run API is called.

- Persist media references with event receipt/sync, staged checksums before use,
  transcripts before admission, and private output snapshots before outbox creation.
- Accept only local homeserver `mxc://` media, authenticated downloads, no redirects,
  no arbitrary URL fetches, authenticated v2 encrypted attachments, no shell interpolation.
- The member write scope explicitly expands to `data/matrix-media/`; all private
  snapshots/model files remain under the bridge's existing `data/controller/` mount.
- Bound files to 20 MiB, combined turn media to 100 MiB, outgoing attachments to five,
  and managed member/private media to 1 GiB per agent. Do not evict pinned recovery
  files to make room. Model storage and bounded transient audio are separate.
- Terminal-turn files expire after seven days; queued/active/held turns and pending
  attachment deliveries stay pinned. Resumed history may refer to expired files.
- Voice is English-only Parakeet v2, maximum five minutes, one recording at a time,
  local-only, with subprocess time/CPU/address-space limits. Recognition is fallible;
  no confidence score, translation or confirmation UI is claimed.
- Only explicit filenames in the originating turn's outgoing folder can be sent;
  reject links, non-regular files, unsafe names and known managed credentials.
- Persist uploaded MXC IDs before sending messages; stable message transaction IDs
  minimise duplicate visible delivery. A crash before saving an upload response can
  orphan/repeat a server media object without repeating the Hermes turn.

## 8. Hermes command parity

The Matrix bridge must preserve important Telegram/Hermes operations rather than assuming raw message delivery is equivalent. The controller may parse explicit text commands in Element where a Telegram-style slash command UI does not exist.

Prioritize and test:

| Command / operation | Required behavior |
| --- | --- |
| `!menu`, `!help` | Fresh reaction controls; command text available under More → Command help |
| `!status` | Queue/run/held/reply status without waking the agent, plus the agent's live state (version, gateway state, platforms, today's counters) when it is running |
| `!stop` | Request cancellation of this conversation's run and cancel its queue; leave other rooms running; never claim tool effects were undone |
| `!steer <text>` | Inject guidance into this conversation's running turn through the Hermes run-steer API; refuse when no turn is running |
| `!retry` | Re-submit this conversation's last recorded prompt as a new turn; tool effects from the earlier attempt are not undone |
| `!version` | The agent's Hermes version from the live agent health endpoint; say plainly when the agent is stopped |
| `!restart` | Administrator only: ask the agent's Hermes gateway to drain and restart itself through its own control socket, wait for the new gateway, and report whether it became ready again; queued work and history survive |
| `!models [filter]`, `!model <model> [provider]` | Paginated model/provider choices, configured inventory with route fallback; finish with This chat / Global scope. Global updates all bridge-managed conversations/default for this agent, not Hermes global config; submitted payloads remain immutable |
| `!new`, `!reset`, `!resume [filter]`, `!conversations [filter]` | Non-destructive, idle-only room conversation switching and filtered inventory |
| `!context` | Report what the Hermes API exposes about this conversation's context: provider-reported cumulative counters, plus the stated limit (live occupancy and headroom are computed inside the gateway and no supported surface exposes them). No fabricated gauge, no internal-file scraping; may wake Hermes |
| `!usage` | Verified cumulative session counters, separately labelled as not context occupancy; may wake Hermes |
| `!approve <request-id>`, `!deny <request-id>` | This originating room/thread/session/run/exact request only; approve once or deny, never blanket grants |
| `!require_mention on\|off` | Reply mode in additional rooms; dedicated room fixed to Always responds. Persisted per agent/room, commands/menu reactions exempt except an unaddressed command in a room shared by several fleet bots, which is refused with a pointer to the intended agent (`!stop` stays global) |
| Background tasks | Only activity observable through the Hermes API contributes to idle safety; no arbitrary job-administration UI |

**Mandatory feasibility gate:** Check the *actual Hermes HTTP API* for model selection, context inspection, cancellation, and approvals; do not assume Hermes native Matrix adapter features are automatically available to an API-based bridge. Unsupported commands must explain the limitation, not fake success. Model, context and stop are high-priority integration acceptance items.

**Command parity with Telegram is a standing checklist, not a one-off decision.**
Hermes is the chat platform in Telegram and an HTTP-API server here, so a Telegram
command is only available in Matrix when the bridge can serve it from a supported
surface (the HTTP API, or the agent's own gateway control socket). The mapping of
every gateway-available Hermes command to its Matrix status, the surfaces behind it
and the re-check procedure live in [command-parity.md](command-parity.md); a Hermes
upgrade is re-checked against it.

The MVP uses plain-text `!` commands because clients can intercept `/` commands. Those commands were delivered through Matrix in the integration checks. Status/cancellation/approvals are processed outside the blocked turn queue, and queued controls form a barrier before further dispatch in that conversation.

Reaction menus target Element X mobile and bundled Element Web desktop without
client or homeserver modifications, widgets, type-ahead or modal dialogs. The bot
seeds numbered reaction chips; lists have five choices plus previous/next/cancel.
Choices are single-use and bound to the opener, room, selected conversation and
relevant settings revision. Stale choices are refused; menus/action intent and
delivery progress live in the same atomic state file. Automatic approval menus
remain usable when navigation changes, but are bound to the originating sender,
turn and exact pending request, reverified over the authenticated API before
approve-once/deny. Reaction removal does not undo actions. Retain direct controls
as fallback; no native command dispatch by sending command text as a model prompt.

Bound storage to 200 recent/pending menus per agent and 1,000 sessions per room;
navigation menus expire after 24 hours, and closed-menu records can be evicted to
keep the bound. Inventory reads consider at most 1,000 advertised models/routes.
Actual Element X tap behavior is a separate acceptance check, not yet verified.

## 9. Security and access control

- Disable federation and public self-registration for v1; private Matrix homeserver only.
- Pre-provision human accounts. Ordinary chat does not require administrator rights, but the configured initial human is deliberately a server administrator by operator choice; disclose the increased browser-session privileges.
- Private rooms, minimum necessary Matrix power levels, and each bot scoped to the rooms it was invited to.
- Room membership is the access boundary: only members of a room the bot has joined can wake that agent, and only an approved human's invitation adds a room. Bot accounts and bot-originated events never wake an agent, so agents cannot loop or be woken by each other. Anyone who can invite the bot into a room chooses where that agent runs, so who may join the operator's rooms is part of the security boundary.
- HTTPS via external reverse proxy; normal Matrix password authentication, rate limits and no anonymous bootstrap exposure.
- **Room E2EE**: bots persist their own device identities, cross-signing and Olm/Megolm state. Human device verification and recovery use normal Element flows. The bridge/Hermes/provider handle plaintext after decryption; E2EE does not protect against compromise of the shared bridge host. See [encryption.md](encryption.md).
- Never expose admin passwords, bot tokens, secrets or agent API keys through Element configuration, room messages or logs.
- Restrict `./data/secrets` permissions; manage credentials without overwriting working credentials on restarts.
- Docker socket access confers near-host-root privileges. Keep the controller trusted and private; do not expose a raw Docker API or interpolate untrusted room text into shell/Compose arguments.
- Validate/normalize configured paths; fail closed if an unapproved message attempts to start a container.

## 10. V1 boundaries

**In scope:** One bundled homeserver + Element + Go controller; automated account, room and membership provisioning; one agent room per configured Compose directory plus every room or space an approved human invites the bot to; URL/login/password-only Web users; durable message relay; wake/idle stop; room-level conversation selection; retained legacy thread history/recovery; reaction controls; typing indicator; text replies and explicit image/file exchange; local English voice-note transcription; per-room mention policy; basic health; verified commands where APIs exist. Native mobile client login is controlled by that client, not bundled Element settings.

**Out of scope:** Federation, public signup, arbitrary external Matrix servers, agent-to-agent autonomous loops, cross-host orchestration, agent cron scheduling, external-job safety, video processing, generated voice replies, speech translation, custom chat UI, E2EE, multi-tenant SaaS, community-space management (curating spaces, room ordering and restricted-join rules on the operator's behalf), and comprehensive Matrix administration tools.

## 11. Acceptance tests

1. `docker compose up -d` starts **one** application service containing Matrix, Element and controller. One hostname serves the UI and Matrix client endpoints.
2. A new human user logs in with the provisioned credentials and sees accessible agent rooms without manual registration, room joins or mentions. Complete standard client account/device verification and recovery setup as needed; confirm existing sessions remain usable after reopening. Fresh devices may require verification.
3. Three configured Compose directories create exactly three stable agent identities and visible private rooms.
4. Posting text to a stopped agent wakes the right Compose project and produces one logical Hermes turn and one correctly routed reply.
5. Restart the bridge between receipt and Hermes startup: the persisted event resumes without duplicate run.
6. New/Resume preserves distinct persisted conversations in a room, refuses busy switches, and preserves legacy thread mappings while rejecting new thread prompts.
7. Typing/activity is visible as supported; active runs prevent idle shutdown; inactive agents stop at the timeout.
8. `!menu`, `!help` and `!status` operate. Reaction choices, model chat/global scope, context/usage distinctions and cancellation pass fake-endpoint tests; real API/client checks have explicit status and blockers.
9. Bridge restart does not duplicate accounts or rooms. Adding a directory provisions a new agent; removing it does not delete message history.
10. Messages from bot accounts or from rooms the bot has not joined cannot wake Hermes or access private agent rooms; a member of a joined room can.
11. An agent invited to a room posts one greeting naming that room and its reply mode, answers there when mentioned, answers every message without a mention when the invited room is a direct conversation with one other human, answers without a mention after `!require_mention off`, and never answers in a space; an invitation pending at startup is accepted on the next start. When several agents' bots share one room, an unaddressed command is refused with a pointer to the intended agent, `!stop` still acts, and a single-bot room is unaffected; a message addressed to another agent is ignored by the others wherever it appears.
12. Automated tests cover initialization failure, permission errors, cold starts, durability, deduplication, recovery and the state upgrade.
13. Fleet's confirmed permanent deletion erases the agent's directory, containers, credentials and its own Matrix room with its messages: the room leaves the operator's room list, its local alias is released, and saved templates, other copies and the Fleet room are untouched. A failed room purge holds the operation, keeps dispatch disabled and completes on retry.

## 12. Technical feasibility spikes before implementation

These are staged verification gates, not all prerequisites before any code can
exist. Provisioning, packaging and API contracts precede the main relay;
controller crash tests accompany the controller. Current evidence is in
`docs/verification.md`; production DNS/TLS/proxy/provider validation remains a
deployment check.

1. **Tuwunel provisioning:** Confirm current supported registration/admin tooling, stable server signing/storage, room creation, and keeping registration closed through bootstrap.
2. **Single-origin hosting:** Confirm Element static assets, Matrix discovery and `/_matrix/*` routing on one hostname through Nginx Proxy Manager, plus Android/desktop compatibility.
3. **Hermes integration:** Inspect authenticated health/run/status/session APIs, model/context/stop support, idempotency semantics, and persistent agent settings.
4. **Threads and activity:** Verify Matrix event relations and replies using current Element; establish typing behavior in threads.
5. **Crash consistency:** Test restart during event receipt, cold startup, Hermes running and outbound reply.
6. **Image packaging:** Pin compatible Tuwunel/Element versions, supervise all processes, preserve licensing notices and test upgrades.

## 13. Implementation order

1. Prove **single-container Matrix + Element** and complete login-only end-user flow.
2. Implement **scripted provisioning** from existing Hermes Compose project directories.
3. Implement **durable controller-owned Matrix-to-Hermes HTTP queue** and container start/stop.
4. Complete **thread isolation and reply/typing routing**, with access controls and restart recovery already present in the first relay rather than deferred hardening.
5. Implement **verified Hermes command parity** and document any unsupported operations.

**Product principle:** The end user should experience a preconfigured chat application, not a Matrix deployment project.

## 14. MVP implementation limits

- One approved human login; additional human provisioning is not implemented, though any member of a room the bot has joined can use the agent there.
- Go 1.27.2; standard library plus approved mautrix-go and SQLite dependencies for E2EE; all local Go tooling runs through the Docker wrapper with the `goolm` build tag.
- Text/root limit 16 KiB; at most 100 pending turns per agent and 100 served rooms per agent; a 50-message per-room sync timeline; UTF-8-safe 12,000-byte reply chunks; 8 MiB remote JSON response bound; 64 MiB controller-state bound; 10,000-event backfill ceiling. Safety-limit failures do not silently acknowledge lost work.
- Three admission attempts; eight visibility failures before holding; eight failed reply sends before explicit retry. Successful long-running jobs continue polling.
- Complete authenticated activity counters, including approval waits/process completions/delegations, are required. Unobservable activity never authorizes shutdown.
- No production proxy changes, provider calls, live fleet mutations, host package installs or Telegram bridge changes are part of the isolated MVP verification.
