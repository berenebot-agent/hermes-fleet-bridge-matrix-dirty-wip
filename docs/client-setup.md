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

## Room menu panel

Every room the bridge serves — agent rooms and the Fleet room — also carries an
HTML menu panel, published as a room widget. Open it from the room's
**Extensions** card in Element Web's right panel. It shows the open menu as
buttons, and a press does what the matching reaction chip does.

The panel needs no extra setup and no separate login: it runs inside Element and
uses the session you are already signed in with. Clients without widget support
(Element X and other apps) do not show the panel and keep the reaction menu, so
nothing is lost by using either surface. The panel is authorised per room, and
only the person who opened a menu can act on it.
