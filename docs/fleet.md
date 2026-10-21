# Fleet management

Fleet management is optional and runs inside the existing controller: no new
service or Go dependency. When enabled, startup provisions a private **Fleet**
room and joins the configured administrator automatically. Only that exact
administrator, in that room's main timeline, can invoke fleet controls. Being
allowed to chat with an agent does not grant fleet-administration permissions.

## Enable managed storage

Back up the stopped bridge and existing member data before changing deployment
configuration. Do not move running agents or their writable storage.

Create two separate directories owned by `HFB_UID`, preferably under the
installation's persistent `data/` directory. For the installed layout:

```sh
mkdir -p /opt/hermes-matrix/data/agents /opt/hermes-matrix/data/templates
chmod 700 /opt/hermes-matrix/data/agents /opt/hermes-matrix/data/templates
```

Set these in the installation's private `.env`:

```dotenv
COMPOSE_FILE=docker-compose.yml:docker-compose.fleet.yml
FLEET_DIR=/opt/hermes-matrix/data/agents
FLEET_TEMPLATE_DIR=/opt/hermes-matrix/data/templates
HERMES_DIRS=/opt/hermes-matrix/data/agents/iris,/opt/hermes-matrix/data/agents/lyra
```

Use the actual absolute host paths, not `/data`, unless that is genuinely the
host installation path. The override mounts each parent directory at its same
absolute host path, so Compose and the host Docker daemon agree on bind sources.
It refuses to auto-create missing bind sources.

**Remove individual member mounts from your local Compose configuration when
enabling the parent mount.** Independent mounts at an agent directory, or below
it, prevent safe deletion and are rejected. The repository's main Compose file
is unchanged; use the optional override rather than adding a second service.

Each existing agent must be a direct child of `FLEET_DIR`, with its own `data/`
bound to `/opt/data`. Writable persistent bind sources for **every service** in
its Compose project must stay inside that agent's directory. Named/anonymous
Compose volumes and symlink escapes are rejected. Operator-configured external
read-only bind resources can remain, but are not copied or deleted. Agent and
template directory ancestors must not be symlinks.

Existing image/API requirements still apply: prebuilt images, supported Hermes
run/idempotency/health APIs, and no Compose environment overrides of the four
bridge-managed `API_SERVER_*` settings. Initial enrolment uses `HERMES_OVERRIDES`
where needed; the discovered identity and endpoint are then persisted.

Rebuild/recreate from the installation directory, after configuring the override:

```sh
docker compose down && docker compose up -d --build
```

`HERMES_DIRS` enrolls existing agents. Agents created in Fleet are recorded in
controller state and survive bridge restarts without editing this variable or
adding mounts. Arbitrary directories are not automatically enrolled. A deleted
agent's stale `HERMES_DIRS` entry does not resurrect it. Removing an entry does
not delete or disable an already-managed agent: use Fleet controls instead.

Managed storage paths are persisted as ownership boundaries. Changing/unsetting
them later fails closed; it is not an implicit migration or reset. Backups must
include bridge `data/`, managed agents, and templates together if those directories
are outside the bridge's usual data bind. This release does not move existing
agents or rewrite the installation's `.env` automatically.

## Compact menu

Send `!menu` or `!fleet` in the Fleet room:

```text
Fleet
├── Agents (paginated)
│   └── Selected agent
│       ├── Refresh status
│       ├── Start
│       ├── Shutdown
│       ├── Pause / Enable
│       ├── Copy agent
│       ├── Save as template
│       ├── Delete
│       └── Back
├── Templates (paginated; Clean or Full snapshot)
│   └── Selected template → creation command example
└── Help
```

All agent actions are top-level; there is no More submenu. Copy and Save show
command examples for entering the destination name. Delete starts a confirmation,
not deletion. Reactions are single-use and bound to the operator, room and stored
agent identity. An old menu cannot target a new agent reusing an old name.

Commands:

```text
!fleet agents
!fleet templates
!fleet status iris
!fleet start iris
!fleet shutdown iris
!fleet pause iris
!fleet enable iris
!fleet create nova from basic
!fleet copy iris to nova
!fleet save-template iris as iris-snapshot
!fleet delete iris
!fleet cancel
!fleet retry
```

- **Start:** explicitly wake an available agent; does not bypass Pause. The
  normal idle timer applies after startup.
- **Shutdown:** drain admitted work and take the Compose project down. Remain
  available: the next message can start it again.
- **Pause:** block new work, drain admitted work, take the project down, and
  persist the paused state. Enable restores message-triggered wake without
  immediately starting containers.
- Existing **`!stop`** in an agent room still cancels conversation work, not
  containers. Status and text cancellation/approval/recovery commands remain
  available while draining; new ordinary work is refused, not silently queued.

Only one fleet operation runs at a time. `!fleet` shows its phase and any error.
While waiting, `!fleet cancel` clears the operation and its admission block.
Once stopping/copying/deleting begins, cancellation is refused. An approval wait,
held turn, or nonzero/incomplete authenticated activity counters can keep it
waiting indefinitely: resolve those deliberately. There is no automatic force
kill. Fix a held operation's reported problem locally, then use `!fleet retry`.

## Clean and full-snapshot templates

A clean template is an operator-created folder under `FLEET_TEMPLATE_DIR` with a
supported Compose file, an existing `data/` directory, securely preconfigured
provider credentials/settings, and this regular manifest:

```json
{"Kind":"Clean"}
```

Save the manifest as `fleet-template.json`. Never paste credentials into Matrix.
Use a clean initial home, not a live agent home labeled Clean. Templates appear
in the menu without restarting the bridge. Creation validates Compose/storage;
a missing/broken template does not become a runnable agent.

`!fleet save-template iris as iris-snapshot` creates a **Full snapshot** template.
It copies all persistent files, including memories, conversation databases,
instructions, skills, provider credentials and integration settings. It is an
independent copy: deleting the source agent does not remove it. Templates are
never overwritten. Template editing/removal is host-side in V1.

Full copies also retain bridge model defaults and bridge-known conversation
mappings. Conversations from the source's rooms are imported into the new agent's
dedicated-room inventory; its current dedicated-room conversation becomes the
copy's default, backed by the independently copied Hermes home. A combined
inventory above 1,000 conversations is refused rather than silently truncated.
Matrix memberships, queued/in-flight work, reply outboxes and menus are not
cloned: those belong to the original operational identity.

`!fleet copy iris to nova` makes the same full copy directly into a new agent.
The controller blocks new source work, drains admitted work, takes the project
down, verifies its containers are gone, and snapshots its files. If the source
was running, it is restarted after publication/provisioning succeeds; its previous
paused/available setting is retained. Failures leave the source guarded until
retry succeeds, rather than silently resuming while the snapshot is incomplete.

Clean-template agents are available immediately for the first message to wake.
Full copies and agents created from Full snapshot templates start **paused**.
Enabling them is an explicit decision: copied schedules, hooks, credentials and
external-chat integrations may act on the same accounts as the source when run.
The bridge cannot guarantee safety for activity outside the Hermes API.

Copies get a fresh internal identity, Matrix bot/room, Compose project and Hermes
API key. Their display name is the requested new name; names use up to 40 lowercase
letters/digits/dots/dashes/underscores and start with a letter or digit. Generated
Compose JSON relocates project-local paths, removes fixed container names and
assigns project-owned network names. Hermes API ports are allocated from 18642
upwards, avoiding live fleet assignments; unmanaged host-port collisions can still
cause readiness failure. Published service ports use fresh Docker-assigned ports,
not the source's fixed host ports. Copies sharing external Compose networks or
volumes are refused; use project-owned networking instead.

Snapshots reject sockets, devices, broken/external symlinks and independent nested
mounts, rather than pretending to copy them. Internal symlinks are preserved and
relocated. Files/directories are private to the bridge user; images must be able
to use that user's persistent home. Existing containers' writable layers are not
persistent state and are not copied.

## Permanent deletion and retained Matrix history

Delete shows the agent and exact scope, then requires a command like:

```text
!fleet confirm-delete iris <short-lived-code>
```

The confirmation expires after five minutes and is bound to the operator and
specific internal identity, not just the reusable display name. Wrong names,
codes, senders, rooms or stale identities cannot confirm deletion.

After admitted work drains, the controller validates project ownership and
storage, takes the project down, and verifies no project containers remain. It
durably retires dispatch before erasing the agent directory. Deletion is retried
idempotently after a partial failure/restart; it never crosses the managed parent,
follows a symlink root or erases another project's storage. No archive, restore,
force-stop or built-in undo is provided.

The bot is removed from active Fleet menus. The controller uses Tuwunel 1.9.2's
administrator deactivation endpoint with `erase: false`, retires the account from
future bridge login/recreation and removes its bridge-held API/account credentials.
If remote deactivation fails, completion explicitly reports that it was unconfirmed;
dispatch is still permanently disabled. A host-side administrator can reconcile
that Matrix account separately.

**Retained:** Matrix rooms, DMs, messages, historical references, saved templates,
other full copies and shared Docker images. Deactivation clears the bot's profile
and leaves its rooms, not the operator's rooms. It cannot promise disappearance
from every Element cache, historical member list or existing DM.

A deleted display name can be reused after a warning and a separate short-lived
confirmation (`confirm-create` or `confirm-copy`). The replacement gets a fresh
internal identity and room; it never resumes the deleted identity's Matrix
history. Controller tombstones retain identity/room mappings but not agent payloads
or credentials.

## Recovery and limits

The operation and phase live in the single atomic controller state file (version
5, upgraded from existing versions). Snapshot files are synced before a directory
is published. Ownership records identify published/staged copies so retries never
overwrite an unrelated destination. A crash in the tiny interval between staging
directory creation and its ownership record can require local inspection/removal
of that empty staging directory; recovery fails closed rather than guessing.
Keep adequate disk space for a complete second copy. Host administrators remain
trusted not to move/change mounted directories during an operation.

V1 manages local Compose agents only. The separate Managed ownership flag leaves
room for future connected-only agents outside managed storage; merely connecting
an agent must not grant Docker or filesystem-deletion authority. Remote enrolment,
Matrix-history cleanup, template editing UI and guided creation wizards are not
implemented.

Tests use fake Compose, Docker, Matrix and Hermes endpoints. They cover flat
menus, operator/room restrictions, confirmation expiry, drain/health safety,
pause persistence, credential separation, clean/full creation, snapshot retries,
publication and deletion crash recovery, name reuse, and storage/mount boundaries.
They do not operate on a real fleet, provider or homeserver.
