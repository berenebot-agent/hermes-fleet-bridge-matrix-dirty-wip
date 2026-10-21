# Hermes command parity (Telegram → Matrix)

The Telegram bridge talks to Hermes as a **chat platform client**: Hermes itself is
the Telegram surface, so a `/command` message is intercepted by the gateway and
handled in-process (`gateway/slash_commands.py`). The Matrix bridge cannot do that:
Hermes is not the Matrix platform. The bridge is a client of the Hermes **HTTP API**
(`gateway/platforms/api_server.py`), and that platform runs the agent directly
(`agent.run_conversation`) — it never dispatches slash commands. Every Telegram
command therefore has to be re-implemented by the bridge, and only the ones with an
external surface can be.

This file is the checklist that keeps the two in step. Re-run it when Hermes is
upgraded: the API grows (jobs, session fork/chat/model, run steer), and commands that
were reported as unavailable become implementable.

Verified against **Hermes 0.21.6** (code `818c13be`) and the bridge as of this
commit.

## Surfaces the bridge may use

| Surface | What it offers |
| --- | --- |
| `GET /v1/capabilities` | The feature flags of the running agent (`run_steer`, `session_fork`, `session_model_lock`, `jobs_admin`, `memory_write_api`, `admin_config_rw`, …) — the authority on what is supported |
| `GET /health/detailed` | Version, gateway state, platform states, today's counters, readiness checks |
| `GET /api/sessions`, `/{id}`, `/{id}/messages` | Session inventory, cumulative counters, the recorded transcript |
| `POST /api/sessions`, `PATCH /{id}`, `POST /{id}/fork`, `POST /{id}/chat`, `POST /{id}/model` | Session create/retitle/fork/chat/model lock |
| `POST /v1/runs`, `GET /v1/runs/{id}`, `POST /v1/runs/{id}/stop`, `/steer`, `/approval` | Run submission, status, cancellation, steering, approvals |
| `GET /api/jobs`, `GET /v1/toolsets` | Background jobs, tool inventory |
| Gateway control socket (`<member home>/gateway.sock`) | `identify`, `status`, `pause-for-update` (Hermes' own drain-and-restart), `reload-plugins`, profile verbs |

There is **no** HTTP command dispatcher and **no** context-window endpoint. The
native `/context` report is computed inside the gateway from the live agent context
(`gateway/slash_commands_status.py`); the API exposes cumulative counters only, and
`/api/sessions/{id}/messages` returns `token_count: null`.

## Parity table

The 69 gateway-available Hermes commands (`hermes_cli/commands.py`, everything that
is not `cli_only`) fall into three groups.

### Covered by the bridge

| Hermes | Matrix |
| --- | --- |
| `/new` | `!new`, `!reset` |
| `/resume`, `/sessions` | `!resume`, `!conversations` |
| `/model` | `!models`, `!model` |
| `/status` | `!status` |
| `/help`, `/commands` | `!help`, `!menu` |
| `/usage` | `!usage` |
| `/stop` (running turn) | `!stop` |
| `/approve`, `/deny` | `!approve`, `!deny` |

### Implementable now, and implemented

| Hermes | Matrix | Surface |
| --- | --- | --- |
| `/restart` | `!restart` | Control socket `pause-for-update`: the gateway drains and exits, the container's restart policy brings it back |
| `/steer` | `!steer <text>` | `POST /v1/runs/{id}/steer` |
| `/retry` | `!retry` | `GET /api/sessions/{id}/messages` for the last prompt, re-submitted as a new turn |
| `/version` | `!version` | `GET /health/detailed` |
| `/platforms`, `/agents` | folded into `!status` | `GET /health/detailed` |
| `/context` (native view) | `!context` | Reports the API's cumulative counters and names the limit; live occupancy stays unavailable |

### Not implementable from outside the gateway

These need gateway-internal state, chat-platform concepts, or local-only tooling.
The bridge must name the limit instead of faking the command. Nothing in this group
is dispatched to the agent as a prompt: a prompt that pretends to be a command is a
fake success.

`start`, `topic`, `save`, `undo`, `title`, `branch`, `compress`, `rollback`, `pause`,
`bg`, `btw`, `journey`, `queue`, `goal`, `heartbeat`, `refine`, `review`, `loop`,
`plan`, `initiate-setup`, `moa`, `subgoal`, `egress`, `whoami`, `profile`,
`sethome`, `codex-runtime`, `personality`, `diff`, `footer`, `yolo`, `approvals`,
`reasoning`, `fast`, `voice`, `busy`, `memory`, `bundles`, `learn`, `init`,
`suggestions`, `blueprint`, `curator`, `kanban`, `reload-mcp`, `reload-skills`,
`login`, `topup`, `insights`, `platform`, `update`, `debug`

Why each group is closed:

- **Gateway-internal state**: `undo`, `compress`, `rollback`, `queue`, `goal`,
  `subgoal`, `heartbeat`, `loop`, `memory`, `journey`, `kanban`, `curator` — no API
  route rewrites a session's history or the gateway's own queues.
- **Configuration writes**: `personality`, `footer`, `yolo`, `approvals`, `fast`
  (global), `config` — `admin_config_rw` is `false`.
- **Chat-platform concepts**: `topic`, `sethome`, `platform`, `start` — they describe
  Hermes' own channels; the Matrix bridge is not one.
- **Local-only tooling**: `diff`, `init`, `learn`, `plan`, `refine`, `review`,
  `suggestions`, `blueprint`, `save`, `egress`, `reload-mcp`, `reload-skills` —
  workspace commands an operator runs on the host.
- **Account-level**: `login`, `topup`, `insights`, `debug`.
- **Prompt-shaped**: `btw`, `moa`, `initiate-setup`, `busy`, `reasoning`,
  `bundles`, `voice`, `codex-runtime` — they either need a second agent/session the
  API cannot create on demand, or they are agent-level behaviour that must not be
  simulated by relaying the words to a model.

## Known differences

- A Hermes command typed in Matrix with a leading `/` is **not** a command: Element
  sends it as text and the bridge delivers it to the agent as a prompt. Use the `!`
  commands above.
- `!context` reports cumulative counters; live context occupancy and headroom are
  gateway-internal and stay unavailable.
- `!restart` restarts the agent's gateway process. It does not recreate the
  container, so a changed bridge-managed `data/.env` is not re-applied by it.

## Re-check procedure

1. Read the agent's live capabilities: `GET /v1/capabilities` and compare the
   feature flags with the table above.
2. Check the route table for new surfaces: `gateway/platforms/api_server.py`
   (`_http_route_table`) and the control-socket verbs
   (`gateway/control_socket.py`, `verb_handlers` in `gateway/run.py`).
3. Compare `hermes_cli/commands.py` for new gateway-available commands.
4. Update this file, the README command list and the spec table in the same change.
