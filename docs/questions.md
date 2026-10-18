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
