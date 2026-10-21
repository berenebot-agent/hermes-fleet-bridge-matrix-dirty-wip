# Fleet management

Fleet management is enabled by the default Compose deployment and runs inside
the existing controller: no new service or Go dependency. Startup provisions a private **Fleet**
room and joins the configured administrator automatically. Only that exact
administrator, in that room's main timeline, can invoke fleet controls. Being
allowed to chat with an agent does not grant fleet-administration permissions.

## Enable managed storage

Back up the stopped bridge and existing member data before changing deployment
configuration. Do not move running agents or their writable storage.

Create two separate directories owned by `HFB_UID` under the installation's
persistent `data/` directory. From the installation directory:

```sh
mkdir -p data/fleet data/templates
chmod 700 data/fleet data/templates
```

The main Compose file defaults to `$PWD/data/fleet` and `$PWD/data/templates`.
Run it from the installation directory. Explicit absolute paths in the private
`.env` override those defaults; for the installed layout:

```dotenv
FLEET_DIR=/opt/hermes-matrix/data/fleet
FLEET_TEMPLATE_DIR=/opt/hermes-matrix/data/templates
```

Use the actual absolute host paths, not `/data`, unless that is genuinely the
host installation path. The main Compose file mounts each parent directory at its same
absolute host path, so Compose and the host Docker daemon agree on bind sources.
It refuses to auto-create missing bind sources.

**Remove individual member mounts from your local Compose configuration when
using the parent mount.** Independent mounts at an agent directory, or below
it, prevent safe deletion and are rejected. The former optional
`docker-compose.fleet.yml` is superseded by the main Compose file; remove any old
`COMPOSE_FILE` setting that references it.

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

Rebuild/recreate from the installation directory:

```sh
docker compose down && docker compose up -d --build
```

Startup scans immediate, non-hidden agent directories under `FLEET_DIR`.
`HERMES_DIRS` is no longer used; remove it from installation configuration.
Each directory must contain a supported Compose project and its own `data/`.
Invalid directories, symlinks, duplicate identities/projects and colliding host
API ports fail startup rather than enrolling an unsafe fleet. Keep unrelated
files outside this directory. Hidden staging directories and pending operation
destinations are left to durable operation recovery.

An adopted directory keeps its own Compose file, so two names must not collide
with anything else on the host: its Compose project (the directory basename
unless the Compose file sets one) and any fixed `container_name` in the service
definition. Docker container names are global, so a copy of an agent that still
runs elsewhere keeps the source's `container_name` and then fails to start with
an opaque Compose error. Rename the container or drop the line before
adopting.

Controller state remains authoritative for existing identities, names, pause
state and conversation mappings. Newly discovered directories recover their
identity from `.fleet-owner.json` when present, or use the directory basename
for legacy agents. A malformed owner file is an error, not a reason to assign
a different identity. Discovery also recovers the persisted API port, optional
availability record and snapshot settings. Existing API keys are preserved.
Deleted members are not resurrected; use Fleet's confirmed creation flow to
reuse a deleted name. Removing a directory by hand does not delete an agent:
use Fleet controls instead.

Templates are already discovered from immediate template directories under
`FLEET_TEMPLATE_DIR` with valid `fleet-template.json` manifests. They are recipes
or snapshots, not containers, and discovery never starts them.

For an explicitly authorised clean Matrix reset, stop the bridge and reset
`data/matrix/`, `data/controller/` and `data/secrets/` together, keeping fleet,
templates and deployment configuration. This loses Matrix history, credentials,
queued work and controller conversation mappings. Back up first if any of those
are needed. On bootstrap agents are rediscovered, bots and private rooms are
provisioned anew, and Hermes homes remain intact. Old room IDs/bookmarks do not
survive. Restored snapshot settings are not a replacement for current controller
state: display names and recent conversation choices may be lost in a reset.

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
│       ├── Rename agent
│       ├── More · Lifecycle
│       │   ├── Start
│       │   ├── Shutdown
│       │   ├── Pause / Enable
│       │   ├── Delete
│       │   └── Back to agent
│       ├── Copy agent
│       ├── Save as template
│       └── Back
├── Templates (paginated; Clean or Full snapshot)
│   └── Selected template → creation command example
└── Help
```

Refresh status, Rename, Copy and Save as template stay on the agent screen.
Start, Shutdown, Pause/Enable and Delete live under **More · Lifecycle**.
Rename, Copy, Save as template and the template list **ask for the new name**: the bridge posts

```text
Copy purple-shirt-1 → reply with the new agent name.
(or: !fleet copy purple-shirt-1 to <new-name>)
Cancels with !fleet cancel · expires in 5 minutes
```

and the operator replies with just the name. An invalid name is explained and the
prompt stays alive, so a typo costs one message; `!fleet cancel` ends it. The
equivalent command still works and is shown as a fallback. Only the operator can
answer, thread replies are refused, and a pending confirmation blocks a new
prompt, so exactly one interaction is outstanding at a time. Delete starts a
confirmation, not deletion. Reactions are single-use and bound to the operator,
room and stored agent identity. An old menu cannot target a new agent reusing an
old name.

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
!fleet rename iris to nova
!fleet delete iris
!fleet cancel
!fleet retry
```

A plain reply in this room is ignored unless a name prompt is pending, so the
room stays quiet during normal use.

- **Start:** explicitly wake an available agent; does not bypass Pause. The
  normal idle timer applies after startup.
- **Shutdown:** drain admitted work and take the Compose project down. Remain
  available: the next message can start it again.
- **Pause:** block new work, drain admitted work, take the project down, and
  persist the paused state. Enable restores message-triggered wake without
  immediately starting containers.
- The agent's own room shows the container lifecycle: it announces the start and
  the readiness, and the shutdown a Fleet stop performs. A deleted agent is not
  told, because its outbox and account are removed with it.
- Existing **`!stop`** in an agent room still cancels conversation work, not
  containers. Status and text cancellation/approval/recovery commands remain
  available while draining; new ordinary work is refused, not silently queued.

Only one fleet operation runs at a time. `!fleet` shows its phase and any error.
While waiting, `!fleet cancel` clears the operation and its admission block.
Once stopping/copying/deleting begins, cancellation is refused. An approval wait,
held turn, or nonzero/incomplete authenticated activity counters can keep it
waiting indefinitely: resolve those deliberately. There is no automatic force
kill. Fix a held operation's reported problem locally, then use `!fleet retry`.

## Rename an agent

Select **Rename agent**, reply with the new name, or send
`!fleet rename iris to nova`. Names use the same lowercase, up-to-40-character
format as creation. Names used by another live or deleted identity are refused
for renaming; choose another name rather than confusing retained history.

Rename changes the agent's Fleet label, Matrix bot display name and **dedicated
room name**. Additional shared rooms keep their names. Internal identity, bot
account ID, room ID/alias, messages, conversations, API key, Compose project and
agent directory all stay unchanged. No container stop/start or draining is needed;
the current running/stopped and paused/available state is preserved. The saved name
survives bridge restarts. Future typed Fleet commands and shared-room display-name
addresses use the new name; the immutable Matrix account handle still works.

Rename is recorded durably before either Matrix request. Profile and room updates
are not atomic: if one fails, their visible names can temporarily differ. The Fleet
room reports the held operation; use `!fleet retry` to finish it. Restart retains
the operation, and the bridge label changes only when both updates succeed.
Rename cannot be cancelled once queued, does not block agent chat, and does not
overwrite either source or destination directories. An older controller cannot
resume a pending rename; finish it before downgrading.

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

### The `vanilla` template

`data/templates/vanilla` is a ready-to-edit clean template: the fleet's
Compose shape, the shared provider credential, and a minimal Hermes home
(`config.yaml`, `SOUL.md`, an empty `memories/USER.md`, an empty
`channel_directory.json`) with no memories, histories or credentials of its
own beyond the provider key.

```text
data/templates/vanilla/
├── docker-compose.yml    # the shape every created agent gets
├── .env                  # provider credentials, shared by every copy
├── .env.example          # the same, without the secret
├── fleet-template.json   # {"Kind":"Clean"}
└── data/                 # the Hermes home
```

It lives under `data/`, so it is **not** tracked in git — it carries a live
provider credential. Back it up with the rest of `data/`, and recreate it from
the recipe below if it is lost.

Edit it host-side; the bridge re-reads templates on every menu open, so
nothing needs restarting. Then create agents from it in the Fleet room:

```text
!fleet create nova from vanilla
```

Two things to know:

- **The provider credential is shared.** Every agent created from this
  template uses the same `TILLER_ROUTER_KEY` from `.env`. Change that file to
  change the credential set for future creations; existing agents keep the
  copy they were created with. The bridge never reads or rewrites `.env`; it
  only writes its own four `API_SERVER_*` settings into each agent's
  `data/.env`, with a fresh random key per agent.
- **Leave `API_SERVER_PORT` out of the template.** The bridge allocates each
  new agent its own port (18642 upwards) and writes it. A port set in the
  template would be applied to every agent created from it.

A newly created agent needs no Hermes onboarding: the home is already
configured and the credential is in place. A **baked** template (see
[baking a template](#baking-a-template)) also carries its installed Python
environment, so agents created from it are ready in seconds. An unbaked
template still works, but its first start installs those dependencies and can
exceed the bridge's `START_TIMEOUT`; the operation is then held and
`!fleet retry` completes it once the container is ready.

To rebuild the template from scratch, copy an existing agent's Compose file,
`.env` and Hermes config, then write the manifest and a minimal home:

```sh
cd /opt/hermes-matrix
mkdir -p data/templates/vanilla/data/memories
cp data/fleet/<agent>/docker-compose.yml data/templates/vanilla/
cp data/fleet/<agent>/.env data/templates/vanilla/
cp data/fleet/<agent>/.env.example data/templates/vanilla/
cp data/fleet/<agent>/data/config.yaml data/templates/vanilla/data/
printf '{"Kind":"Clean"}\n' > data/templates/vanilla/fleet-template.json
chmod 600 data/templates/vanilla/.env
```

Drop the `container_name` line from the copied Compose file and leave out
`API_SERVER_*` keys. `data/config.yaml` carries the provider and model
settings but no secret: the provider key comes from `.env` via
`providers.<name>.key_env`. Copy `data/SOUL.md` and a `data/memories/USER.md`
too if the new agent should have a persona or a user profile; leave them out
for a bare agent.

### Baking a template

A template is an agent's persistent directory, so it can carry the agent's
**installed tooling** as well as its configuration. Baking means running that
directory once so Hermes installs its Python environment into it, then stopping
the container: the directory is the template, and agents created from it start
in seconds rather than installing on first use.

```sh
cd /opt/hermes-matrix

# From an existing agent: copy a stopped agent's directory into a new template.
./bake-fleet-template.sh data/fleet/purple-shirt-1 my-template

# Or warm a template that already exists.
./bake-fleet-template.sh --warm my-template
```

The copy is verbatim: files, modes and symlinks as they are, with the source's
own `.fleet-owner.json` and template manifest left behind. Live sockets and
devices are runtime state and are not copied. `--warm` uses the template's own
`docker-compose.yml`, publishes nothing, provisions no Matrix account and stops
the project afterwards, so the template has no room or identity of its own.

Baked templates are larger than bare ones because they contain the dependency
environment, and every agent created from one carries its own copy. Budget for
that on the data volume.

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

Clean-template agents, full copies and agents created from Full snapshot templates
all start **ready to receive work**: the first message in the new agent's room
wakes it. A copy carries the source's schedules, hooks, credentials and
external-chat integrations, so the copy and the source may act on the same
accounts once both run. The bridge cannot guarantee safety for activity outside
the Hermes API.

Copies get a fresh internal identity, Matrix bot/room, Compose project and Hermes
API key. Their display name is the requested new name; names use up to 40 lowercase
letters/digits/dots/dashes/underscores and start with a letter or digit. Generated
Compose JSON relocates project-local paths, removes fixed container names and
assigns project-owned network names. Hermes API ports are allocated from 18642
upwards, avoiding live fleet assignments; unmanaged host-port collisions can still
cause readiness failure. Published service ports use fresh Docker-assigned ports,
not the source's fixed host ports. Copies sharing external Compose networks or
volumes are refused; use project-owned networking instead.

A copy reproduces the agent's directory as `cp -a` would: regular files, their
modes and every symlink travel as they are, hard links are preserved and links
pointing inside the tree follow the copy. Socket, device and fifo files are
runtime state rather than agent data, so they are skipped the way `tar` and
`docker cp` skip them instead of failing the copy. Independent nested mounts are
still refused: mount only the fleet/template parents. Files/directories are
private to the bridge user; images must be able to use that user's persistent
home. Existing containers' writable layers are not persistent state and are not
copied.

Because the copy is faithful, a template can carry a **baked** Hermes
environment — the venv, cached packages and plugin state that otherwise have to
be installed on first start. `./bake-fleet-template.sh` copies a stopped agent
into a template, and `./bake-fleet-template.sh --warm <name>` boots an existing
template once so its tooling installs, then stops it. Neither provisions Matrix:
no account, room or bridge involvement. A baked template starts its agents in
seconds instead of performing the install on first use, and it contains no live
sockets afterwards.

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

Tests use fake Compose, Docker, Matrix and Hermes endpoints. They cover grouped
menus, operator/room restrictions, confirmation expiry, drain/health safety,
pause persistence, credential separation, clean/full creation, snapshot retries,
publication and deletion crash recovery, name reuse, storage/mount boundaries,
and the name prompt: copy, create-from-template, invalid-name recovery, expiry,
cancellation, refusal while a confirmation is pending, and plain text being
admitted only while a prompt is pending. Rename coverage includes partial Matrix
failure/retry, durable recovery, restart reconciliation, name collisions,
permissions and shared-room addressing. They do not operate on a real fleet,
provider or homeserver.
