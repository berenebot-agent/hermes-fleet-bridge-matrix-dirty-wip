# Changing the public URL or administrator password

The private `.env` is operator-controlled configuration. Changes apply at startup;
the controller does not watch the file live. Recreate the service, rather than
using `docker compose restart`, to pass new environment values to the process.

## Public URL versus Matrix identity

`PUBLIC_URL` is the HTTPS origin serving Element and Matrix client endpoints.
`MATRIX_SERVER_NAME` is the permanent suffix of Matrix user IDs and room aliases.
They can differ. For example:

```dotenv
PUBLIC_URL=https://fleet.example.com
MATRIX_SERVER_NAME=original.example.com
```

Users open `fleet.example.com`, log in with their existing username/password and
see the same rooms/history. Their full Matrix IDs still end in
`:original.example.com`. The controller's Element configuration and client
discovery advertise the new public origin while retaining the original Matrix
identity. All browser/client requests still use the one public HTTPS origin;
the homeserver and administrator APIs remain private loopback listeners.

To change the public URL:

1. Configure the new hostname's DNS, TLS certificate and existing reverse proxy
   to route the whole origin to this deployment's frontend port.
2. Change **only `PUBLIC_URL`** in the private `.env`. Keep the original explicit
   `MATRIX_SERVER_NAME`, even if the new public hostname differs. If omitted, the
   server name defaults to the URL hostname on initial configuration, so pin it
   before changing URLs on an existing installation.
3. Back up the stopped deployment and recreate/rebuild from the installation
   directory:

   ```sh
   docker compose down && docker compose up -d --build
   ```

The controller validates the HTTPS origin and atomically updates its stored
public-URL metadata. Accounts, Matrix data, signing keys, room mappings, queues,
Hermes run IDs and agent sessions are not renamed or reset. A restart after
adopting the URL is idempotent. An unavailable DNS/proxy/certificate is not fixed
by the controller; inspect local health separately from public reachability.

A browser's storage belongs to its origin, so a new URL can require a fresh login.
Old-origin browser sessions are not automatically transferred or revoked. Keep the
old proxy alias only if you intend it to remain usable.

**Changing `MATRIX_SERVER_NAME` is not a URL migration.** Tuwunel 1.9.2 documents
that its server name cannot change without a database wipe. The bridge continues
to reject that change, as well as a changed `ADMIN_USERNAME`; it never rewrites
historical identities or creates replacement accounts to hide a mismatch. Restore
the original values rather than deleting controller state or Matrix data.

## Administrator password

To change the login password, set `ADMIN_PASSWORD` to the desired new value in
the private `.env`, keeping `ADMIN_USERNAME` and `MATRIX_SERVER_NAME` unchanged.
Use at least 16 characters, with no line breaks. Never paste a password into
Matrix, logs, a shell command line, an issue, or a tracked file.

On startup, when the configured password differs from the last stored private
credential, the controller:

- Tries the desired password first, recognizing an already-applied remote change.
- Otherwise authenticates the existing account with its stored token or password
  and verifies its exact user ID.
- Calls Tuwunel's private authenticated
  `POST /_synapse/admin/v1/reset_password/{user_id}` with `logout_devices: true`.
- Logs in using the desired password, verifies the resulting identity through
  `whoami`, and atomically saves the verified credential in `data/secrets`.

When the bridge applies a reset, existing administrator devices/tokens are revoked,
including browser sessions; the bridge signs back in itself. Bot passwords, Hermes
API keys, administrator identity, memberships and history remain unchanged. If the
desired password was already applied elsewhere or by a previous attempt, it is
verified/saved without issuing another reset. Unchanged settings do not cause
additional logins or resets when the stored token is still valid.

The endpoint is supported by the pinned Tuwunel 1.9.2 source. This requires existing
administrator authentication; shared-secret account registration is not used as a
password-reset back door. Public registration remains closed and no administrative
endpoint is exposed through the public frontend.

### Interrupted or failed password changes

A remote change can succeed even if its response, the verifying login or the local
secrets write fails. In that case the previous private credential record is retained
and startup reports a generic failure without including secrets. **Keep the same
desired `ADMIN_PASSWORD` and retry** after fixing connectivity/storage: the next
startup can log in with the already-applied password and finish saving it without
resetting it again. Do not assume the old password still works or revert settings
mid-recovery. No automatic remote rollback is attempted.

If neither the stored credential nor the desired credential can authenticate,
startup fails closed. It never silently replaces the account or weakens access
controls to recover it. Updating the public URL and password together is supported,
but does not remove these authentication and storage requirements.

Tests use local fake endpoints for password resets, token expiration, lost responses,
failed verification, failed secrets writes, restart recovery and identity mismatches.
No external provider is involved.
