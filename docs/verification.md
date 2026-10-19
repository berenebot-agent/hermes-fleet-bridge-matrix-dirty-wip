# MVP verification

Review/build date: **2026-10-19**. Host UTC test logs are dated 2026-10-18.
Tests were explicitly authorized. No subagents were used.

## Exact artifacts

- Tuwunel `v1.9.2`, image digest
  `sha256:a354976db35cb6e3f8c0b801f266592571af898554d78fc6e2137f14e02a7d3e`.
- Element Web `v1.12.13`, image digest
  `sha256:5107e63026c13ed014f743e485821b7d4b56d275a41e76303859bb14f5f94eb6`.
- Go `1.27.2-bookworm`, image digest
  `sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61`.
- Docker CLI `29.9.1-cli`, image digest
  `sha256:6fae2214c3121a28d5aa639d09032f61310b7c9c75ffbad1025f261a1b621800`;
  bundled Compose reports `5.6.0`.
- Real Hermes test image:
  `nousresearch/hermes-agent@sha256:9774f4f39a9bb8c2f68ce728ed5e99ddbad282163be56764afacf88ed952b784`.

Application images contain the controller, static Element assets, Tuwunel and
Docker CLI/Compose in one service. Third-party licences are copied with pinned
checksums and served under `/licenses/`. No third-party Go modules.

## Reaction menus and room conversations (2026-10-19)

The latest change adds bridge-owned reaction menus, not Element/Tuwunel changes.
Passed through the Docker wrapper: `gofmt -l .`, `vet ./...`, `build .`, and the
full local `test -race ./...` suite. The final race log is
`tests/logs/hermes-fleet-bridge-matrix-go/20261019T015944-go-test_-race_._...-1634580.log`.
`tests/integration.py` also passed a Python AST syntax check without execution.
Local fake-endpoint checks cover menu owner/room binding, unrelated/bot reactions,
duplicate sync delivery, stale/expired choices, paginated model/provider choices,
chat/global model scope with immutable submitted requests, New/Resume and busy-room
refusal, restart of selected conversations and resolved choices, automatic exact
approval menus, menu/reaction outbox retries with stable transaction IDs, mention
exemption, fixed dedicated-room reply mode, and reaction Stop while a model read is
blocked. Thread prompts are now rejected; old mappings/recovery remain supported.
State version 3 migrates versions 1/2 without changing existing session IDs, and
prevents older controllers from silently ignoring selected conversations on rollback.
Back up the whole stopped data directory before upgrading; a downgrade requires a
matching controller/state backup, not manually changing the version number.

Upstream source was inspected on GitHub, not the local Hermes installation:
[`NousResearch/hermes-agent` at `67481b6`](https://github.com/NousResearch/hermes-agent/tree/67481b6f37615e58f1434ba6a1528e4c6e9b754c).

- `gateway/platforms/api_server_runs.py`: explicit `session_id` requests resume
  persisted conversation history, resolve compression-continuation tips, and
  expose session identity in run status. The bridge retains a returned completion
  session ID when present, keeping its own registry key stable.
- `gateway/slash_commands_status.py`: the native `/context` handler reports
  window use/headroom/compression, with provider-reported versus estimated counts.
- `gateway/platforms/api_server.py`: its session representation exposes cumulative
  counters, not those live context fields; the HTTP route table does not expose
  `/context` or a general command dispatcher.
- `gateway/session_commands.py`: a separate session command dispatcher includes
  context reads, but requires local-source authorization; that is not evidence of
  a supported HTTP command path for these API sessions.

Accordingly, `!context` reports the blocker without waking Hermes. `!usage` is the
separate cumulative-counter read. No Hermes changes, local-state scraping or
prompt-based command impersonation were introduced.

**Not run for this change:** real-image integration, a live deployment rebuild,
real providers, Element Web interaction, or Element X phone tapping/login. The
integration harness has been updated for reaction choices, room-level New/Resume,
thread rejection and model scope, but its new paths are not claimed as verified.
Element X is the mobile acceptance target, not a tested compatibility guarantee.
The real-image/browser results below are historical, from before this menu change.

## Passed local checks before reaction menus

`gofmt`, `go vet ./...` and `go test -race ./...` ran through the Docker wrapper.
The final race run is recorded in
`tests/logs/hermes-fleet-bridge-matrix-go/20261019T012932-go-test_-race_._...-1555332.log`.

Fake-endpoint tests cover:

- Invalid URL/identity/settings and credential-safe error messages.
- Dotenv preservation, existing keys, quoted multiline values, duplicates,
  weak keys, symlink rejection and private permissions.
- Single-writer locking, identity mismatch, corrupt/missing state, write failure
  poisoning, and retention-window downgrade handling.
- Approved-sender/known-room routing, bot/edit filtering, transactional queue/cursor
  receipt, redelivery deduplication, limited-sync backfill ordering and gap refusal.
- Room-membership routing: any human member of a joined room dispatches, `agent_*`
  bot senders and their events never do, and a thread-root from another agent is
  refused in the agent's own room but skipped in an operator-created room.
- Per-room mention policy: no mention needed in the provisioned room, mention needed
  in an operator-created room, mention stripped before dispatch, commands exempt, and
  `!require_mention on|off` persisting the setting per room.
- Shared-room commands: with two agents' bots in one room, a bare command is refused
  with one explanation naming the agent, an addressed command and `!stop` still
  dispatch, and a single-bot room and the agent's own room are unaffected.
- Space containers are tracked and joined but never dispatched from.
- Invitation handling: a human invitation joins, a bot invitation never joins, and
  startup reconciliation accepts a pending invitation without moving the sync cursor.
- Replies for a room the bot has left are dropped instead of blocking the outbox head.
- A history gap in an operator-created room restarts that room without wedging the
  other rooms' sync.
- State version 1 upgrades in place to version 2 (room cursor, session map, turn and
  notice rooms) and reloads; an unknown state version is refused.
- Queue backpressure without blocking safety commands; stable per-thread sessions.
- Lost Matrix acknowledgement with identical transaction IDs; Unicode chunking.
- Frontend admin/federation blocking and refusal to show the UI before readiness.
- Parallel sessions with FIFO in a session; known-run restart without resubmission;
  lost admission acknowledgement with immutable payload/key; expired-window holds.
- Conversation-scoped cancellation and exact-request approve-once boundaries.
- Safe release of an expired run record only after authenticated global inactivity.
- Session-scoped model/provider selection and inventory rendering.
- Idle shutdown blocked by background completions; ambiguous running replicas
  never trigger Compose up/recreation.
- Crash-time room-alias adoption without duplicate creation; saved room IDs do
  not depend on aliases remaining present.
- A rejected first thread turn retains its quoted root context for an explicitly
  resent turn after the held request is safely discarded.
- Typing starts before cold readiness; status/stop remain responsive while an
  agent is starting. Lifecycle waits and outbound Matrix sends do not occupy the
  dispatch/control loop; cancelled runs cannot receive a new approval.
- Stale dispatch snapshots do not wake completed/cancelled turns or turn them
  into false uncertain-outcome holds, including a late startup failure after a
  queued turn was already cancelled.
- The wrapper's format gate fails when `gofmt -l` lists files, but ignores Docker
  image-pull messages on stderr when the formatter reports no files.
- Escaped quote characters inside quoted multiline member `.env` values are
  respected, so managed-looking lines inside unrelated values are never rewritten.

## Passed real-image checks before reaction menus

`tests/integration.py` uses generated scratch Compose projects and bind state,
never existing fleet directories. Its local fake inference server is reachable
only through the scratch internal network's host gateway. Agent networks are
internal: no real provider credentials or Internet inference access.

The packaged application was run non-root, read-only, with `/tmp` tmpfs,
`cap_drop: ALL`, `no-new-privileges`, a 768 MiB memory cap, and the Docker socket
limited in controller logic to configured scratch projects.

Verified against real Tuwunel, Element assets, Docker and Hermes:

1. Closed public signup; private shared-secret provisioning; automatic joined
   membership; three stable private agent rooms visible to the human login.
2. Public admin API is blocked. Internal listeners stay on loopback.
3. A stopped agent cold-wakes, accepts a real run through the actual API, calls
   only the local fake provider, and returns final text to Matrix.
4. Bridge restart during a slow real run resumes the existing run without a
   second inference request or a duplicate room.
5. Distinct thread/session IDs and quoted root context.
6. A genuine terminal-tool approval is surfaced in Matrix and its exact request
   is denied; no destructive action executes. Approve-once/cross-thread rejection
   is separately tested with fake endpoints.
7. Real run cancellation and cancellation of queued work in that conversation.
8. Help/status/model inventory/context paths, plus an actual subsequent inference
   request using the newly selected model/provider.
9. Authenticated idle checks stop the Compose project without deleting bind state.
10. Removing two agents retains their rooms/history while disabling dispatch.

### Operator-created rooms and spaces (2026-10-19, preceding build)

Verified against the running local deployment on the same Tuwunel/Element pins, with
three provisioned agents. State was written by the previous build, so this exercised
the version 1 to 2 upgrade on real data.

1. The upgrade rewrote controller state to version 2 in place: per-room cursors,
   session maps keyed by room, and turn/notice rooms defaulted to the provisioned
   room. No history or session identity changed.
2. Reconciliation granted the human administrator power level 100 in each previously
   provisioned room (`users` now lists the administrator; the bot remains the room
   creator). Renaming a room, setting a topic and inviting a user, all previously
   refused with `M_FORBIDDEN`/power level 0, now succeed.
3. Spaces nest agent rooms again. Adding an agent room to an operator-created space
   writes `m.space.child` in the space and `m.space.parent` in the room; both returned
   200, where the parent write previously failed and Element rolled the link back.
4. Invitations pending from before this build were accepted on the next start
   (startup reconciliation), and the bots appeared as joined members of the room and
   of the space. An invitation from another bot was refused.
5. A plain room message produced no turn; the same room with a mention produced one
   turn whose text no longer carried the address, the agent cold-woke, ran, and
   replied into that room. `!require_mention off` was confirmed in-room, after which a
   plain message dispatched and answered; the setting was restored to required.
6. Rooms the bot left, unknown rooms and spaces are not dispatched from; the space is
   joined and tracked with `Space: true`.

Residual risk unchanged: anyone who can invite the bot into a room chooses where that
agent runs, and any member of a room it has joined can use the agent there.

A separate Hermes feasibility spike also restarted **Hermes itself** after a
completed request and recovered the same run ID/final output by replaying its
key, without another fake inference call. This does not promise that an
interrupted tool run can safely execute again: such runs are held.

Private generated results live under gitignored `tests/logs/integration-*/`.
Expected provider disconnections during a cancellation are not a failed run.

## Browser acceptance

`tests/browser-check.cjs` uses already installed Playwright/Chromium, not a new
runtime dependency. It reads generated credentials from private fixture files;
passwords are not passed through visible tool arguments or printed.

The real Element bundle was loaded in an isolated browser context and exercised
with the generated **username/password** (not a pre-injected session token).
Verified provisioned-room visibility, access to final reply history, opening a
thread and its messages, and no required device-verification/recovery wizard.

The test proxies its HTTPS origin inside the browser to the scratch HTTP
frontend. Therefore it **does not prove your DNS, TLS certificate, NPM rules or
service-worker behavior through your real proxy**. A synthetic service-worker
warning may be dismissed in that intercepted test environment; media is outside
the bridge's V1 scope. Verify the real HTTPS origin at deployment.

The browser test also paginates older room history before opening a thread, so a
longer test transcript is not mistaken for a missing thread conversation.

An early browser-harness shutdown error printed a disposable scratch access
token. Its scratch administrator sessions were explicitly revoked and the
disposable containers removed; no production credential was involved. The
harness now catches closing long-poll failures without dumping request headers.

## Resource sample

One idle sample, with three provisioned agent rooms and all Hermes agents stopped:
**74.18 MiB** for the complete bundle, **0.15% CPU** in `docker stats`, against
the 768 MiB cap. This is a point measurement, not a load/performance guarantee.

## Not established by these checks

- Production hostname, public DNS, trusted TLS/NPM setup or real provider access.
- Every native Matrix client or mobile notification/background behavior.
- Exactly-once visible Matrix replies after remote transaction-cache expiry.
- Long-duration load, large fleets, arbitrary Compose layouts, migrations between
  homeserver releases, or rollback after a RocksDB upgrade.
- Safety for cron, external chat adapters, unmanaged concurrent tools or workloads
  not observable through the Hermes API.
- Automated credential rotation or additional human-user provisioning.
- Space curation on the operator's behalf (ordering, sub-spaces, restricted-join
  membership) and joining rooms created *inside* a space the bot is in: the operator
  invites the bot to each room it should serve.

Those are not grounds to bypass fail-closed behavior. Deploy only bridge-managed
agents, preserve consistent backups, and use `README.md` for the operator setup.
