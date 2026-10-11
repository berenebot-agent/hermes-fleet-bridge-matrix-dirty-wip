# Hermes Matrix Fleet Bridge — Functional Specification

**Version:** 0.2 (design draft)  
**Date:** 2026-10-11  
**Project:** `hermes-fleet-bridge-matrix-dirty-wip`  
**Reference implementation:** [Telegram Hermes Fleet Bridge](https://github.com/berenebot-agent/hermes-fleet-bridge-dirty-wip)

## 1. Purpose and non-negotiable outcome

Build a **standalone, self-hosted Matrix chat platform for a fleet of existing Hermes agents**. It replaces Telegram as the chat surface, while carrying forward the existing fleet bridge's primary job: automatically start Hermes containers on a message and stop them when idle.

**Non-negotiable user experience:** The end user receives **one HTTPS URL, a username, and a password**. They sign in and immediately see their provisioned agent rooms. They must **not** need to choose/configure a homeserver, register, create rooms, accept invitations, change permissions, find bot IDs, type `@mentions`, install plugins, manage access tokens, or complete device verification.

The *operator* performs one-time deployment by supplying the hostname, account credentials, agent Compose directories, and Docker access. **Everything relating to Matrix configuration, accounts, bots, rooms, permissions, and Element setup happens automatically.**

Keep this project entirely separate from the existing Telegram bridge. Do not alter the Telegram bridge repository.

## 2. Architecture

One Docker image / one Docker Compose service bundles:

| Component | Proposed implementation | Function |
| --- | --- | --- |
| Matrix homeserver | Tuwunel | Accounts, private rooms, history, sync, threads |
| Web chat client | Element Web static assets | Browser UI; optionally use standard Element Android/desktop clients |
| Fleet controller | Go | Provision users/rooms, receive Matrix events, durable dispatch to Hermes API, control Compose lifecycle |
| HTTP frontend | Internal Go HTTP server/reverse proxy | One public hostname for Element and `/_matrix/*` routes |

- **One external HTTPS URL.** TLS terminates at an existing reverse proxy such as Nginx Proxy Manager.
- **One exposed application service.** Internal homeserver and controller listeners are not published separately.
- **One persistent `./data` bind mount** for Matrix, controller state, and protected secrets.
- No external PostgreSQL, Redis, hosted service, separate Element container, or additional manual Matrix administrator is required.
- Existing Hermes agent containers and persistent directories remain outside this application.

**Message transport decision for initial build:** The always-running bridge controller owns Matrix bot identities and Matrix event consumption. It queues messages and relays them to Hermes through its existing authenticated HTTP run API. **Do not simultaneously enable a Hermes native Matrix client for the same room/bot**, or duplicate consumption/replies may result. This design deliberately avoids depending on a stopped native Matrix adapter to wake and recover the first incoming event.

Treat the precise Tuwunel version, noninteractive user creation mechanism, Element compatibility, and Hermes API contracts as implementation checks, not assumed capabilities.

## 3. Packaging and deployment

Normal installation layout:

```text
/opt/hermes-matrix/
├── docker-compose.yml
├── .env
└── data/
    ├── matrix/
    ├── controller/
    └── secrets/
```

Illustrative operator configuration (**not yet implemented**):

```dotenv
PUBLIC_URL=https://chat.example.com
ADMIN_USERNAME=admin
ADMIN_PASSWORD=replace-with-strong-secret
HERMES_DIRS=/srv/hermes/iris,/srv/hermes/hermes-dev,/srv/hermes/hermes-prod
IDLE_TIMEOUT=15m
START_TIMEOUT=120s
```

Agent membership is defined by a comma-separated list of existing **Docker Compose project directories**, consistent with the Telegram bridge. Mount each directory into the bridge at the *same absolute host path* so relative bind mounts in the member's Compose project resolve correctly. Also mount the Docker socket and durable `./data`.

Allow optional overrides for display name, Compose project/container, network selection and Hermes API port when inference fails. Do not require such overrides in the normal case.

On first start validate public hostname, administrator credentials, mounted directories, Docker/Compose access, and secure storage permissions. Reject invalid configuration explicitly. Never silently change the Matrix server name after accounts have been created.

## 4. Completely automated bootstrap

When `./data` is empty, the entrypoint/controller must:

1. Validate required inputs and permissions.
2. Generate and persist stable Matrix homeserver configuration, signing keys, registration/admin bootstrap secrets, and controller credentials.
3. Start the bundled homeserver with **supported noninteractive provisioning interfaces**, without temporarily exposing public registration.
4. Create the initial human administrator account from deployment settings.
5. Disable public registration and federation; restrict the homeserver to approved users.
6. Generate Element configuration pointing at the bundled homeserver, removing homeserver selection / account-creation friction where the chosen client allows.
7. Discover and validate configured Hermes Compose projects.
8. Provision a Matrix bot account and private room for each agent, with correct permissions and automatic administrator membership.
9. Save stable mappings, room IDs, event offsets, credentials and queue state with safe filesystem permissions.
10. Mark the application ready only when the user can actually log in and see the agent rooms.

On every subsequent startup, reconcile missing accounts, rooms and invitations **idempotently**. Do not reset histories, reset passwords, duplicate rooms, rotate working credentials unnecessarily, or overwrite unrelated Hermes configuration.

**Acceptance:** A new human user opens the URL, enters the supplied username/password, and immediately sees working agent rooms. No wizard, room join, `@mention`, bot activation or Matrix administrative steps.

## 5. Agent discovery, accounts and rooms

The source of truth is `HERMES_DIRS`, not an incidental list of running containers. Directory basename is the default stable agent ID and room display name.

For each valid project:

- Create/reuse a bot user, e.g. `@iris:chat.example.com`.
- Create/reuse one **private dedicated room**, e.g. **Iris**.
- Automatically join the administrator and that agent's bot; never require manual invitations.
- Map project directory, Compose project, bot account, room ID, API key, and persistent Hermes session IDs.
- Configure or discover the Hermes API key by supported persistent `.env` conventions; preserve existing keys and unrelated configuration.
- Allow discovery/reconciliation after an operator adds a new directory to the configuration.
- Preserve messages, rooms and membership history after an operator removes an agent; simply disable its dispatch until intentionally retired.
- Ignore unknown rooms, unapproved senders, bot-originated events, and messages associated with removed agents.
- Do not require mentions in dedicated agent rooms. Prevent bot-to-bot reply loops.

Additional human users may be supported later; when explicitly provisioned/approved by an operator, they should also receive usable rooms automatically. Do not expose open self-registration.

## 6. Thread-to-session mapping

A dedicated Matrix room is the default conversation surface for each agent. Multiple Matrix **threads within the room** must be usable as independent Hermes conversations.

- Map `(room_id, thread_root_event_id)` to a stable, persisted Hermes session ID.
- Plain messages to a room use a separate default room session.
- Send each reply back into its originating Matrix thread or room.
- Keep message ordering FIFO per session; run parallel agents/sessions only if the Hermes API supports this safely.
- Preserve session identities across bridge and Hermes container restarts.
- Support typing indicators while an agent starts and runs, insofar as Matrix clients display them.
- Treat reactions, edits and attachments as distinct features; never accidentally interpret them as instructions.

## 7. Message handling and lifecycle

**On a new authorized Matrix message:**

1. Receive an event from the always-running controller and validate sender, room, type and thread.
2. **Persist the event ID and queued turn before advancing durable Matrix sync state.**
3. Deduplicate repeat Matrix deliveries and place the turn in the appropriate agent/session FIFO queue.
4. Check the agent Compose project and call `docker compose up -d` if necessary.
5. Wait for authenticated Hermes API readiness, subject to a startup timeout.
6. Create the Hermes run with a stable session and idempotency key where available.
7. Relay typing/activity and any supported streaming/progress; send final answer to the originating room/thread.
8. Persist run and reply progress; acknowledge successful completion logically once.

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
- Exactly-once visible Matrix replies cannot be promised without supported remote idempotency; document residual duplicate risk.
- Bound retries. Report actionable failures within the agent room without leaking credentials.

## 8. Hermes command parity

The Matrix bridge must preserve important Telegram/Hermes operations rather than assuming raw message delivery is equivalent. The controller may parse explicit text commands in Element where a Telegram-style slash command UI does not exist.

Prioritize and test:

| Command / operation | Required behavior |
| --- | --- |
| `/help` | Show commands that actually work |
| `/status` | Agent running/idle, queue/run status and relevant errors |
| `/stop` | Cancel/stop a bridge-owned run **if** Hermes exposes a supported cancellation endpoint |
| `/models`, `/model` | List/select models scoped to session **if** API permits |
| `/context` | Display context/tokens/session usage **if** the API exposes it |
| Approvals/background tasks | Include only with proven secure API/UI support |

**Mandatory feasibility gate:** Check the *actual Hermes HTTP API* for model selection, context inspection, cancellation, and approvals; do not assume Hermes native Matrix adapter features are automatically available to an API-based bridge. Unsupported commands must explain the limitation, not fake success. Model, context and stop are high-priority integration acceptance items.

Command syntax must be tested in Element because a client can intercept slash commands. If literal `/model` is not deliverable as a normal message, choose a reliable alternative (e.g. `!model`) and document it clearly.

## 9. Security and access control

- Disable federation and public self-registration for v1; private Matrix homeserver only.
- Pre-provision human accounts; users never need administrative permissions to chat.
- Private rooms, minimum necessary Matrix power levels, and each bot scoped to its own rooms.
- Only approved users can wake agent containers; reject unknown rooms and remote identities.
- HTTPS via external reverse proxy; normal Matrix password authentication, rate limits and no anonymous bootstrap exposure.
- **No E2EE in v1** to avoid device-verification and bot key management complexity. Clearly disclose that the homeserver stores messages in server-readable form.
- Never expose admin passwords, bot tokens, secrets or agent API keys through Element configuration, room messages or logs.
- Restrict `./data/secrets` permissions; manage credentials without overwriting working credentials on restarts.
- Docker socket access confers near-host-root privileges. Keep the controller trusted and private; do not expose a raw Docker API or interpolate untrusted room text into shell/Compose arguments.
- Validate/normalize configured paths; fail closed if an unapproved message attempts to start a container.

## 10. V1 boundaries

**In scope:** One bundled homeserver + Element + Go controller; automated account, room and membership provisioning; one agent room per configured Compose directory; URL/login/password-only users; durable message relay; wake/idle stop; thread/session isolation; typing indicator; text replies; basic health; verified commands where APIs exist.

**Out of scope:** Federation, public signup, arbitrary external Matrix servers, agent-to-agent autonomous loops, cross-host orchestration, agent cron scheduling, external-job safety, file/audio/media relay, custom chat UI, E2EE, multi-tenant SaaS, and comprehensive Matrix administration tools.

## 11. Acceptance tests

1. `docker compose up -d` starts **one** application service containing Matrix, Element and controller. One hostname serves the UI and Matrix client endpoints.
2. A new human user uses **only** URL + username + password and immediately sees accessible agent rooms. No other Matrix setup, join, mention or verification.
3. Three configured Compose directories create exactly three stable agent identities and visible private rooms.
4. Posting text to a stopped agent wakes the right Compose project and produces one logical Hermes turn and one correctly routed reply.
5. Restart the bridge between receipt and Hermes startup: the persisted event resumes without duplicate run.
6. Two threads in the same room remain distinct persistent Hermes sessions.
7. Typing/activity is visible as supported; active runs prevent idle shutdown; inactive agents stop at the timeout.
8. `/help` and `/status` operate. Model selection, context inspection and cancellation pass real API tests or have explicitly documented blockers and supported alternatives.
9. Bridge restart does not duplicate accounts or rooms. Adding a directory provisions a new agent; removing it does not delete message history.
10. Messages from unauthorized Matrix users or unknown rooms cannot wake Hermes or access private agent rooms.
11. Automated tests cover initialization failure, permission errors, cold starts, durability, deduplication and recovery.

## 12. Technical feasibility spikes before implementation

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
4. Add **thread isolation, reply/typing routing, access controls and restart recovery**.
5. Implement **verified Hermes command parity** and document any unsupported operations.

**Product principle:** The end user should experience a preconfigured chat application, not a Matrix deployment project.
