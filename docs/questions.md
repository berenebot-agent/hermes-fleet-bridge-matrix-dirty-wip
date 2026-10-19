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
- Mention policy is per room, set from chat with `!require_mention on|off`: never
  required in the agent's provisioned room, required by default in rooms the user
  created. Per-agent configuration was rejected as more surface for the same result.
- The bridge grants the human administrator power level 100 in each provisioned room
  and repairs it on reconciliation. Tuwunel's `private_chat` preset leaves an invitee
  at power level 0, which blocks renaming a room and filing it into a space: Element
  rolls its space link back when the parent event is refused.
- Per-room backfill anchors and per-room Hermes sessions; `(room_id, thread_root)`
  identifies one conversation. The provisioned room keeps the strict fail-closed
  history rule; a user-created room may not wedge the agent's other rooms.
- State version 2 upgrades version 1 in place (single room and session map wrapped
  under the provisioned room) rather than requiring a data reset.
- Parallel sessions are supported; FIFO within each session. Agent files/tools
  remain shared. No artificial single-run-per-agent restriction.
- New thread sessions receive quoted root context, not full room-history forks.
- Exact-request approve-once/deny through text commands; no permanent approvals.
- `!context` reports verified usage counters; exact live occupancy is not claimed.
- `!stop` affects active/queued work only in its originating conversation.
- Ambiguous execution holds that session. `!discard` requires confirmed inactivity;
  another attempt must be an explicitly resent message, never automatic replay.
- Removing an agent cancels pending turns, drains active turns, retains history,
  and does not automatically stop a now-unconfigured project.
- `PUBLIC_URL` and `MATRIX_SERVER_NAME` are Compose/.env settings. Initial identity
  is permanent; mismatches are errors, not silent renames or database resets.
- Replace unsupported Go 1.23 with supported, pinned Go 1.27.2 in Docker.

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
