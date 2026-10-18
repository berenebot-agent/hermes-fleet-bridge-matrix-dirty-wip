# Open Questions — Hermes Matrix Fleet Bridge

Status: **pre-implementation.** These questions block a first-pass V1 build of
`hermes-fleet-bridge-matrix-dirty-wip`. Answer, then implementation can start.

Source of truth for the design is `docs/functional-spec.md` (v0.2, design draft).
This file does not restate the spec; it records what the spec leaves open.

Assessment summary: the spec is clear on *what* to build and on the guardrails
(§1/§4 acceptance criteria, §5 discovery, §7 ordering, §9 access control are all
implementable as written). It is not yet clear enough to code against, because
§12's feasibility gates are unwritten and four of them change the design rather
than merely confirming it.

---

## A. Blocking items — the spec defers what an implementer needs first

### A1. Tuwunel noninteractive provisioning is unspecified

§4 step 3 says "supported noninteractive provisioning interfaces" without naming
one. Account creation, bot users, private rooms, power levels and admin
membership all depend on the answer.

Need to establish:
- Whether Tuwunel exposes an admin API or admin CLI, or only the
  registration-shared-secret flow plus the client API.
- How to create the initial admin account without opening public registration
  (§4 step 4 and §4 step 5 are in tension until this is known).
- Whether room creation and power-level assignment can be done by a scripted
  client, and whether that client can be the bot or must be the admin.

This is spec §12 gate 1. Until it is settled, the whole bootstrap design (§4) is
guesswork and cannot be written down.

### A2. Hermes API surface is unverified (spec §8 mandatory feasibility gate)

§7 promises durable dispatch semantics that may not be achievable against the
real API. Need to establish, against the actual Hermes HTTP API:

- Whether runs accept a caller-supplied idempotency key and/or a caller-chosen
  session ID — this is the difference between at-most-once dispatch and a
  best-effort queue.
- Whether genuine cancellation exists (`/stop` parity item).
- Whether model listing/selection and context/token inspection are exposed
  (`/models`, `/model`, `/context` parity items).
- Readiness/health semantics for a cold-started container — what signal means
  "the API is up and will accept a run".

Per §8: unsupported commands must explain the limitation, never fake success.
Model, context and stop are high-priority integration acceptance items.

### A3. Single-service packaging vs the homelab proxy

§2 asserts one published application service and one external HTTPS URL, with
TLS terminating at an existing reverse proxy. Two things need confirmation:

- Same-origin routing of Element static assets and `/_matrix/*` to two internal
  listeners from one published service.
- Networking mode. The Telegram bridge uses `network_mode: host` so it can reach
  each Hermes container's inspected bridge IP; whether that carries over here,
  and whether it is compatible with a single published port behind the proxy, is
  unresolved. (Homelab note: the reverse proxy and the Docker host are separate
  boxes, so a proxied service needs a pinned host port.)

This is spec §12 gate 2.

### A4. Matrix server name is not an explicit setting

`PUBLIC_URL` is required, but the homeserver server-name is not a documented
configuration key — and it is permanently baked in at account creation. The spec
says only "never silently change the Matrix server name after accounts have been
created" (§3).

Need: an explicit, validated setting for the server name, decided before first
bootstrap, with the consequence of getting it wrong stated in the docs.

---

## B. Questions for Ben

1. **V1 scope target.** First pass = spec §13 step 1 only (single container,
   Matrix + Element, login-only, no Hermes integration), or steps 1+2+3 (through
   the durable queue and container lifecycle)? The spec's own order says step 1
   first.
2. **Test environment.** Where do the spike containers run — the same host as the
   Telegram bridge's live fleet, or a throwaway on sonny? This determines whether
   the spike can touch real agent containers.
3. **Domain / hostname.** Is a hostname reserved for this (e.g. a `chat.`
   subdomain) and is it already behind Nginx Proxy Manager, or should the spike
   be login-only on a plain HTTP host port first?
4. **Element client coverage.** Bundled Element Web only, or also verify Element
   Android/desktop against the same hostname in V1? (Spec §12 gate 2 lists
   Android/desktop compatibility.)
5. **Registration lockdown timing.** The spec says disable public registration
   and federation (§4 step 5), but Tuwunel may require registration enabled at
   first boot to create the admin. Acceptable to have registration open **only on
   an internal listener with no public route** during the bootstrap window, then
   closed?

---

## C. What can be settled without Ben (read-only spikes)

These are investigations, not decisions. They can proceed once the corresponding
question above is authorised:

- **C1** = A2: read the Hermes codebase and the live API to establish run/session
  idempotency, stop/cancel, models, context and health semantics.
- **C2** = A1: Tuwunel provisioning — pin the version and establish the
  noninteractive admin/bot/room path without exposing public registration.
- **C3** = A3: the one-service, one-hostname packaging proof through the proxy.
- **C4** = A4: pick and validate the server-name setting shape.

Spec §12 gates 3, 5 and 6 (Hermes integration, crash consistency, image
packaging/licensing) complete the spike set. Gate 4 (Matrix threads and replies
in Element) depends on step 2 above.

---

## D. Recommended path

Green-light spec §13 step 1 only as the first pass, with a written spike report
covering gates 1, 2, 4 and 6 before any controller code. That is a bounded piece
of work, produces something that can actually be logged into, and de-risks the
assumptions the rest of the spec rests on (§13 steps 2–5).

No application code should be written until the §12 gates are resolved and the
answers above are recorded here.

---

## E. Answers

_(record answers here as they arrive; move settled items out of §A/§B and delete
the stale question text)_
