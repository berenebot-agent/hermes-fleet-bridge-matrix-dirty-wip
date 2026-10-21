# Element X and Element Web setup

The deployment uses full room E2EE. See [encryption.md](encryption.md).

## Current encrypted deployment

Log in at the configured HTTPS URL (Web) or select that server in Element X.
Complete normal encryption setup, enable key storage and save the recovery key
in a password manager. It is separate from the account password. Verify later
logins from an existing device or with the recovery key. Do not clear the only
verified device before recovery works.

Agent rooms, including Fleet, are encrypted automatically. Bots keep their own
cryptographic identities across restarts and do not need an Element session.
The Web encryption-suppression module and encryption-disable settings are removed.
New-device login is not password-only. Bot identities are self-cross-signed;
cross-user trust is TOFU, not independently verified contact identity.

After the approved fresh reset, old app sessions refer to deleted accounts and
histories. Remove/sign out of the old session and log in with the configured
credentials. Set up a **new** recovery key for the fresh identity; the previous
deployment's recovery key cannot recover it. Remove stale client account state
before adding the account again if necessary.

For unreadable encrypted messages, keep a verified device signed in, verify the
affected session and check recovery. Do not disable key storage or repeatedly
reset identities to hide warnings: that can permanently lose encrypted history.
