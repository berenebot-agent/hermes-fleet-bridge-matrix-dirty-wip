# Matrix end-to-end encryption

This contract supersedes the earlier unencrypted V1 design. The operator explicitly
approved third-party Matrix libraries, persistent crypto storage and a fresh Matrix
deployment on 2026-10-21. Fleet directories, templates and Hermes homes are not reset.

## Implementation and boundaries

- One bundled Tuwunel + Element + Go service remains. Federation and public signup
  remain disabled. Provisioned rooms enable Megolm in initial state, including Fleet.
- The controller uses pinned mautrix-go, its pure-Go Olm/Megolm implementation
  (`goolm` build tag), and modernc SQLite. This is not home-grown cryptography, nor
  a claim of an independent audit of goolm or this integration.
- Each bot has an isolated database, pickle key and cross-signing seeds in
  `data/secrets/crypto/<agent>/`, and a persistent `HERMES_FLEET_BRIDGE` device.
  Private directories are 0700, files 0600. Device identity and keys stay paired.
- Bot cross-signing is provisioned and the device is self-signed. Human identities,
  key backup and recovery belong to Element; the controller does not hold human
  recovery keys or cross-signing secrets.
- Sender and recipient devices must be cross-signed under trusted-on-first-use
  identities. Unknown, unsigned and blocked sender devices cannot dispatch work.
  This is **TOFU**, not independent verification of every contact. Interactive
  emoji/QR verification with individual bots is not implemented.
- E2EE covers Matrix message contents and attachment bytes. Membership, room names,
  timestamps and standard reaction events remain visible. Reactions remain normal
  Matrix reaction events so existing menus work.
- The `msgtype` is inside the encrypted payload, so it stays readable only by the room
  members. Container start/ready/stop notices use `m.notice`; a client applies its push
  rules to the decrypted event, so the default `.m.rule.suppress_notices` rule keeps
  them out of notifications without any per-user push-rule management.
- The bridge decrypts before sending to Hermes. Queue state, staged media,
  transcripts, Hermes history and provider requests can contain plaintext. Both
  homeserver and controller share a trusted host; host compromise defeats this
  separation. This is not an encrypted-at-rest agent or provider system.

## Receipt and recovery

The controller remains the only bot sync consumer. It processes to-device keys,
device lists and room membership/encryption state through the SDK. The timeline
filter includes encrypted events and room-state changes.

Incoming ciphertext is retained with its sync cursor before acknowledgement.
Messages awaiting keys are retried on later syncs, including quiet rooms and after
restart. The inbox is capped at 1,000 events / 16 MiB per agent. Key requests use
persisted exponential backoff, at most eight per event and eight per sync. After
seven days the bot queues an explanation asking the user to verify and resend.
It never dispatches guessed or partial text.

Decryption checks room binding and sender-device trust. Decrypted events enter the
existing event-ID deduplication, command handling and durable Hermes queue.
Plaintext message events are not admitted in production: no downgrade fallback.
Other bots' encrypted messages are ignored to prevent agent loops.

Outgoing ciphertext is persisted before sending. Retries and restarts reuse the
same ciphertext and Matrix transaction ID. Multipart replies clear that cache only
after a part is acknowledged. Text, menu messages and file-message envelopes use
encrypted events. Existing bounded retries and `!resend` remain; this does not
claim exactly-once remote delivery under all failure modes.

Before the first human login there is no recipient encryption device. Welcome
messages wait for a joined, cross-signed human device without exhausting send
retries. This avoids producing unreadable pre-login welcome history.

## Media

Images/files/voice use Matrix v2 encrypted attachments. Downloads remain local
MXC-only and size-bounded. The ciphertext SHA-256 is checked **before** plaintext
is staged. Outgoing files are encrypted before upload as opaque bytes without the
original filename or MIME. Keys, IV, hash and filename travel inside the encrypted
message. Upload metadata is persisted with the outbox. Existing quotas, path
confinement, credential screening, transcription and retention still apply.

## Rooms, rollout and backup

Provisioned rooms are encrypted from creation. For operator-created rooms, enable
encryption before inviting the bot, or grant it permission to enable encryption.
An unencryptable operator-created room is not served and does not block other rooms.
Spaces remain membership containers, not conversations.

Enabling encryption is a one-way room change and does not encrypt old plaintext
history. A fresh bridge deployment is the supported rollout; E2EE does not repair
previously inconsistent human secret storage. The approved reset archives only
`data/matrix`, `data/controller` and `data/secrets`, preserving fleet/template
directories and Hermes homes. Old queued/held turns are not resumed. Archives still
contain private plaintext and credentials and must remain protected.

Stop the service and back up all `data/` together, including SQLite/WAL files,
secrets and controller state. Restore a consistent set. Do not delete individual
crypto files, reuse a device ID with different keys, edit state versions to
downgrade, or reset bot identities to silence warnings.

## Checks

Local fake-endpoint tests use the real SDK, Olm/Megolm and SQLite: encrypted text
both ways, delayed Olm keys across restart, outbound ciphertext reuse across
restart/lost acknowledgement, encrypted upload/download, room binding, replay and
plaintext refusal, private permissions and attachment integrity. Existing
Matrix/Hermes/Docker tests remain local fakes. Live deployment and phone/Web
acceptance are reported separately in `verification.md`.
