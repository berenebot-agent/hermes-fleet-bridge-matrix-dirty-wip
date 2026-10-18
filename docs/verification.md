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

## Passed local checks

`gofmt`, `go vet ./...` and `go test -race ./...` ran through the Docker wrapper.
The final race run is recorded in
`tests/logs/hermes-fleet-bridge-matrix-go/20261018T234122-go-test_-race_._...-1298900.log`.

Fake-endpoint tests cover:

- Invalid URL/identity/settings and credential-safe error messages.
- Dotenv preservation, existing keys, quoted multiline values, duplicates,
  weak keys, symlink rejection and private permissions.
- Single-writer locking, identity mismatch, corrupt/missing state, write failure
  poisoning, and retention-window downgrade handling.
- Approved-sender/known-room routing, bot/edit filtering, transactional queue/cursor
  receipt, redelivery deduplication, limited-sync backfill ordering and gap refusal.
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

## Passed real-image checks

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

Those are not grounds to bypass fail-closed behavior. Deploy only bridge-managed
agents, preserve consistent backups, and use `README.md` for the operator setup.
