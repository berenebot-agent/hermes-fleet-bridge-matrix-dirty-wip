# Element X and Element Web setup

Rooms remain **unencrypted**: the homeserver stores their messages in
server-readable form. Client account/device verification is a separate concern
from room encryption. Disabling room encryption does not stop clients from
asking to verify devices or to configure key backup.

## What actually needs setting up

Because no room is encrypted and no encrypted history exists, server-side key
backup and recovery protect nothing here. The useful steps are:

1. Log in with the provisioned credentials. Public registration stays disabled.
2. Complete device verification if the client asks. On a fresh account the
   first Element X login normally bootstraps cross-signing and signs its own
   device, which leaves the session verified without an extra step.
3. Leave **key storage / recovery** off. It is not needed for unencrypted rooms.

There is no bridge-side code that enables, disables or repairs client key
storage. It is entirely client account state.

## Element X can turn key storage back on

Element X hardcodes `autoEnableCrossSigning(true)` and `autoEnableBackups(true)`
in `RustMatrixClientFactory`, so a first login may create a backup version and a
secret-storage key even though nothing here uses encryption. It also ignores the
`io.element.e2ee.force_disable` well-known key; that convention is Element Web
only.

If Element X offers key storage, you can switch **Settings → Encryption →
Allow key storage** off. The client then marks backups explicitly disabled
(`m.key_backup.enabled=false`, `m.org.matrix.custom.backup_disabled.disabled=true`),
which stops later logins from re-enabling them automatically. Restart the app
afterwards so the settings screen re-reads the server state.

## Bundled Web skips the encryption setup screen

Element Web shows **Confirm your identity** after login whenever the account has
cross-signing keys. This deployment keeps every room unencrypted, so there is no
encrypted history for that step to protect.

The bridge therefore serves a small Element module (`element_module.go`) and
lists it in `/config.json`'s `modules` array. At startup the module registers a
`cryptoSetup` extension reporting `SHOW_ENCRYPTION_SETUP_UI: false`, and Element's
`postLoginSetup` then goes straight to the room list.

Element's default extension reports that flag as `true`, and the client skips the
prompt only when the flag compares equal to `false` — a real boolean `false` is
what makes the check pass.

The module must also expose an `emit` method. Element calls `emit` on every
registered module for each lifecycle event, and a module without it stops the
client with "Your Element is misconfigured" before the login screen appears.

This is a UI suppression, not a crypto fix: device verification, cross-signing and
key storage remain available in Settings and keep whatever state the clients gave
them. If a future Element build removes the internal registration hook the module
uses, it logs a warning and Element falls back to its stock behaviour — the prompt
returns, nothing else breaks.

Element X is unaffected: it ignores `config.json` entirely and forces
cross-signing and backups on its own.

## "Your key storage is currently out of sync"


This appears when secret storage is enabled but is missing one of the cross-signing
secrets (`m.cross_signing.master`, `self_signing`, `user_signing`). Re-entering a
recovery key does not fix it, because the missing secrets were never stored — the
key unlocks the store, it cannot recreate absent contents. This is a known client
behaviour, tracked upstream as
[element-x-android#4343](https://github.com/element-hq/element-x-android/issues/4343)
and [matrix-rust-sdk#4736](https://github.com/matrix-org/matrix-rust-sdk/issues/4736).

Prefer **disabling key storage** over resetting the cryptographic identity. A
reset is the last resort, because it invalidates the current verification state
and can leave encrypted history unreachable. Do not repeat resets to silence a
prompt.

To confirm the real server state rather than the app's cached view, read the
account data for the affected user: `m.secret_storage.default_key` absent plus
`m.key_backup.enabled=false` means recovery is disabled and out of scope.

## Verifying Web against the phone

Keep the phone signed in and verify the Web session from it (compare the shown
emoji or numbers), or use a recovery key if one was deliberately kept. Verify
only sessions you initiated. Existing signed-in sessions should keep working;
a new login, reinstall or cleared client storage may need verification again.

Bundled Web configuration does not configure an installed Element X app, and no
verification-suppression module ships with this deployment. Actual first-login
and repeat-session behaviour must be checked against the installed app version;
the automated bridge tests do not exercise native clients.
