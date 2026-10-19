# Resolved V1 decisions

The pre-implementation interview is complete. Implementation was explicitly
authorized, without subagents, including isolated Docker/browser checks and the
local fake-endpoint race suite. This file records decisions rather than leaving
stale blocking questions for the next implementer.

- Full V1 design scope, delivered through staged proof gates.
- One image/service: Tuwunel, Element static assets, standard-library Go controller.
- Linux host networking; one frontend port for NPM on another machine; loopback
  homeserver listener. No new external database, auth service or named volumes.
- Human login is a Matrix server administrator (accepted increased privileges).
- Compatible Matrix clients can connect; preconfigured login-only UX is guaranteed
  for bundled Element Web, not every native client's setup screens.
- Room membership is the access boundary, decided by the operator after the first
  live space/room tests: anyone who can post in a room the bot has joined may use
  that agent there, and any approved human's invitation is accepted. Bot accounts
  (`agent_*`) and their events are always ignored so agents cannot wake each other.
  The alternative — an approved-sender allowlist — was rejected as user-level rather
  than entry-level control.
- Users may create spaces and rooms and invite agents into them. Spaces are joined
  but never answered in. Invitations pending at startup are reconciled, because an
  incrementing sync reports invites only relative to its cursor.
- Mention policy is per agent/room, set from chat with `!require_mention on|off` or
  the Reply mode reaction menu: never
  required in the agent's provisioned room, required by default in rooms the user
  created. Per-agent configuration was rejected as more surface for the same result.
- Commands stay mention-exempt, except in a room where several fleet agents' bots are
  members: an unaddressed command is refused with a message naming the sender's
  intended agent, because acting in every bot at once is ambiguous. `!stop` is the
  exception and remains global, stopping every agent's conversation in that room.
  "Several" is derived from the per-agent room records sync already maintains, so no
  extra homeserver call is made; detection is therefore as fresh as each agent's sync.
- The bridge grants the human administrator power level 100 in each provisioned room
  and repairs it on reconciliation. Tuwunel's `private_chat` preset leaves an invitee
  at power level 0, which blocks renaming a room and filing it into a space: Element
  rolls its space link back when the parent event is refused.
- Per-room backfill anchors and a selected Hermes conversation per room, with
  non-destructive New/Resume and bridge-only inventory. The provisioned room keeps the strict fail-closed
  history rule; a user-created room may not wedge the agent's other rooms.
- State version 4 upgrades versions 1/2/3 in place, preserving IDs and adding media
  recovery state alongside menu delivery/action state and selected conversations. Older controllers refuse the
  new version rather than silently routing to the old default conversation.
- Parallel sessions are supported; FIFO within each session. Agent files/tools
  remain shared. No artificial single-run-per-agent restriction.
- New thread prompts/commands are rejected with a main-room explanation. Retain
  old thread mappings/history and recovery; the current interaction is room-only.
- `!menu`/`!help` open single-use, owner/room/conversation-bound reaction controls
  for Element X mobile and bundled Element Web desktop. No client/homeserver forks,
  widgets, type-ahead, modal dialogs or additional services. Native-client login and
  actual Element X tap behavior remain separate acceptance checks.
- Model choice ends with This chat / Global. Global means all this agent's
  bridge-managed conversations and its bridge-side future default, not Hermes
  global config. Submitted request bodies never change.
- New/Resume requires an idle room, including settled controls and delivered
  non-menu replies. Titles use first-prompt excerpts and last-used time; no AI calls.
- Exact-request approve-once/deny through text commands and automatic reaction
  prompts bound to the original sender; no permanent approvals.
- `!context` documents the native context-report HTTP API blocker; `!usage`
  separately reports verified cumulative counters, never labelled as occupancy.
- `!stop` affects active/queued work only in its originating conversation.
- Ambiguous execution holds that session. `!discard` requires confirmed inactivity;
  another attempt must be an explicitly resent message, never automatic replay.
- Removing an agent cancels pending turns, drains active turns, retains history,
  and does not automatically stop a now-unconfigured project.
- `PUBLIC_URL` and `MATRIX_SERVER_NAME` are Compose/.env settings. Initial identity
  is permanent; mismatches are errors, not silent renames or database resets.
- Replace unsupported Go 1.23 with supported, pinned Go 1.27.2 in Docker.
- Explicitly approved media extension: receive/send images and files; receive
  English voice notes and transcribe locally into normal text prompts, without
  Hermes modifications. No generated voice replies or cloud transcription.
- Reuse Parakeet TDT 0.6B v2 INT8 through standalone sherpa-onnx, with FFmpeg for
  decoding. The glibc executable failed on Alpine; Debian Bookworm was then
  explicitly approved. No Node/Python runtime, new Go modules or extra services.
- Extend member write scope only to dedicated `data/matrix-media/` folders;
  restrict output references to explicit filenames in per-turn folders and
  snapshot them privately before durable delivery. No filesystem watcher.
- Limits agreed: 20 MiB/file, 100 MiB/turn, 1 GiB/agent; seven-day terminal retention,
  with queued/active/held turns and pending delivery pinned. Five outgoing files
  and five minutes per voice note; native processing also has resource/time caps.

## Feasibility outcomes

Tuwunel 1.9.2 shared-secret provisioning worked with public registration closed.
Bots create private rooms, invite the configured human internally, and the
controller joins them using the human's authenticated client API. No human has
to accept an invitation. Existing aliases plus ownership state allow recovery
after creation before local mapping persistence.

Rooms created with `preset: private_chat` gave the invited human power level 0, so
the human could not rename a room, invite anyone, or file it into a space. Element's
add-to-space writes `m.space.child` in the space and then `m.space.parent` in the
room, and rolls the child back when the parent write is refused — which is why an
agent room silently stayed in Home. The bot, as room creator, now writes
`users[admin] = 100` into the room's power levels on reconciliation; live checks
confirm renames, invites and space nesting then succeed. Room version 12 creator
power is server-side only, which is why the explicit grant is required: the creator
cannot be named in `users`, but nobody else can be granted power unless the creator
does it.

The actual pinned Hermes image completed a local-fake-provider run, replayed the
same key/run ID, and preserved final output across a Hermes restart. Ordinary
idempotency retention is finite; the bridge requires durable storage and holds
uncertain work outside that window. It never treats source inspection alone as
evidence for a deployed image.

Crash/recovery tests are staged with their implementation, not impossible
prerequisites before any application code. Access control, durable receipt,
uncertain-outcome handling and the reply outbox ship with the first relay.

## Operator inputs still required

Production hostname/TLS/proxy configuration, a private administrator password,
actual member directories/mounts and Docker socket group are deployment inputs,
not unspecified architecture. They were not invented or applied to the live
fleet while the owner was asleep. Follow `README.md`.

Actual production-provider behavior, native clients, upgrade/downgrade migrations
and prolonged load are not covered by the local MVP verification. See
`docs/verification.md` for the evidence and remaining limits.
