---
name: gpuardian
description: Set up GPUardian MCP, reserve/protect, extend, authorize, yield, reclaim, or inspect shared GPU access with GPUardian. Use when the user tags GPUardian/$gpuardian, legacy RocGuard/$rocguard, or asks to setup, configure, protect, reserve, guard, authorize, allow, claim, share, release, yield, hand off, resume, or inspect GPU access. For protect/reserve, require exact GPU IDs, duration in hours, and a purpose; default authorization to the current Linux user; default node to the current GPUardian node; split reservations into 24h-or-smaller chunks while requiring continuous coverage.
---

# GPUardian

Use this skill as a low-friction wrapper around GPUardian. Use the
user-facing name "GPUardian" while accepting legacy `$rocguard` prompts.

The main user flow is:

```text
$gpuardian setup
username: alice
password: ...

$gpuardian protect
GPU: 0,1
duration: 4
purpose: dev session
authorize: current user
node: current node
```

Setup is one-time per Codex user and configures GPUardian MCP automatically.
`GPU`, `duration`, and `purpose` are required for protect/reserve. `duration`
uses hours as the unit. `authorize` defaults to the current Linux user. `node`
defaults to the current GPUardian node.

## Operating Rules

- Treat every `gk_...`, `rk_...`, password, cookie, token, or secret as
  sensitive. Never repeat it in responses, logs, summaries, or examples beyond
  redacted placeholders.
- Never place a password in shell argv, environment examples, pipes, heredocs,
  or final responses. Feed setup passwords through stdin/PTY only.
- Use GPUardian MCP for reservation/protect flows. The MCP server authenticates
  to the web gateway with GPUardian username/password and exposes
  `list_servers`, `fleet_snapshot`, `create_reservation`, and `allow`.
- Do not call `reveal_key`, `regenerate_key`, `revoke`, or admin/root actions
  unless the user explicitly asks and is authorized.
- GPUardian CLI node-side operations read a fixed key from `KEY`. Admin
  operations read the node root key from `ROOT_KEY`.
- Prefer using an existing shell `KEY` when a CLI allow/run flow is needed. If
  the user supplied `GPUARDIAN_KEY` or legacy `ROCGUARD_KEY`, map it to `KEY`
  locally without printing the value.
- The node CLI talks to `GPUARDIAN_SOCKET`, defaulting to `/run/gpuardian.sock`;
  it does not talk to the web gateway.
- Use the narrowest exact authorization scope. Treat wildcard scopes as
  admin-only.
- Do not revoke reservations, rotate keys, kill workloads, or stop containers
  during a yield handoff unless the user explicitly asks and the target is exact.

## Intents

- **Setup**: Configure the local Codex GPUardian MCP connection from
  username/password so future `$gpuardian protect` requests work without manual
  setup.
- **Protect/reserve**: Ensure exact GPU IDs are reserved continuously for the
  requested duration and authorize a scope. This requires `GPU`, `duration`, and
  `purpose`.
- **Authorize-only**: Add an authorization rule for an existing reservation/key
  without creating a new reservation.
- **Yield/share**: Add a recipient authorization without revoking the user's
  reservation or key.
- **Reclaim**: Re-authorize the current user/scope after yielding; do not create
  a new reservation unless the user uses the protect contract with `GPU`,
  `duration`, and `purpose`.
- **Inspect**: Show status, current processes, reservations, keys, or history
  without changing state.

## Setup Workflow

Use this for `$gpuardian setup`.

Inputs:

- `username`: required unless the script will ask interactively.
- `password`: required unless the script will ask interactively. Treat as a
  secret and never echo it.

If the user provides username/password in the prompt, run the bundled setup
script with the username in argv and the password via stdin:

```text
scripts/setup-gpuardian-mcp --username "<username>" --password-stdin
```

Start the command in a PTY/session, then send only the password line to stdin.
Do not use shell pipes, heredocs, inline env vars, or command arguments that
contain the password.

The script uses the fixed gateway URL, clones/updates `nrhevu/GPUardian`,
installs the MCP server under `~/.codex/mcp`, writes the MCP config to
`~/.codex/config.toml` if missing, and stores credentials in
`~/.codex/secrets/gpuardian-mcp.env` with mode `600`.

After setup, say only that GPUardian MCP is configured for the username and that
the user should restart Codex before the first protect request. Do not print the
password or credential file contents.

## Protect Contract

For `$gpuardian protect` or `$gpuardian reserve`, require:

- `GPU`: exact numeric GPU IDs, such as `0`, `0,1`, or `[0, 1]`.
- `duration`: numeric duration in hours, such as `1`, `2`, or `4.5`.
- `purpose`: non-empty reservation purpose.

Optional fields:

- `authorize`: defaults to the current Linux user.
- `node`: defaults to the current GPUardian node.
- `starts_at`: defaults to now. Compute `expires_at` from `starts_at` plus
  `duration` hours.

Reject missing or non-exact GPUs. Do not auto-pick GPUs. Treat `GPU: auto`,
`GPU: any`, or `GPU: 1 GPU` as missing/invalid and ask for exact IDs.

Reject missing, non-numeric, or non-hour duration. Accept `duration: 4` or
`duration: 4h` as 4 hours; reject minute/day units and ask for hours.

If `GPU`, `duration`, or `purpose` is missing, stop before any mutating tool
call and report only the missing fields, for example:

```text
Thiếu GPU IDs. Dùng: GPU: 0 hoặc GPU: 0,1
Thiếu duration. Dùng: duration: 4
Thiếu purpose. Dùng: purpose: dev session
```

## Node Resolution

Resolve `node` before reserving:

1. If the user provides `node`, match it to a GPUardian `server_id`, node name,
   or endpoint from `list_servers`.
2. If `node` is omitted, default to the current node:
   - read local `hostname`, `hostname -f`, and host IPs when available;
   - call `list_servers`;
   - match current hostname/IP against server ID, name, or endpoint host;
   - if there is exactly one registered server, use it;
   - otherwise stop and ask for `node`.
3. Never guess between multiple plausible nodes.

## Authorization Resolution

Resolve `authorize` into one exact `allow` scope:

- Omitted, `current user`, or `user: current` → current Linux user from `id -un`.
- `user: <name>` or `linux user: <name>` → `allow` mode `user`.
- `container: <name-or-id>` or `docker: <name-or-id>` → `allow` mode `docker`.
- `k8s: <namespace>` or `namespace: <namespace>` → `allow` mode `k8s`.

If `authorize` is provided but lacks a clear type, ask for one of:
`user:<name>`, `docker:<container>`, or `k8s:<namespace>`.

## Reservation Limits

- A single `create_reservation` call must request at most 24 hours.
- Requested coverage can exceed 24 hours only when it can be represented as
  multiple back-to-back reservation chunks with no gap.
- If the requested GPUs cannot be reserved continuously for the full requested
  duration, treat protect as failed. Do not present partial coverage as success.
- On failure, report the maximum continuously reservable hours from `starts_at`
  across all requested GPUs on the selected node, plus conflict details visible
  in `fleet_snapshot`.

## Protect Workflow

Use this sequence for `$gpuardian protect`:

1. Validate required fields: exact `GPU` IDs, `duration` in hours, and
   `purpose`.
2. Resolve `node` to `server_id`.
3. Resolve `authorize` to one exact scope.
4. Inspect current state with `fleet_snapshot`.
5. Compute the requested coverage window:
   - `starts_at`: supplied value or now.
   - `target_expires_at`: `starts_at + duration hours`.
6. For every requested GPU, compute existing continuous coverage owned by the
   current GPUardian user from `starts_at`.
7. Compute the maximum continuously reservable hours:
   - start at `starts_at`;
   - include the user's already-owned continuous reservations;
   - then extend through visible free time until the first conflicting
     reservation, active lease, or blocking process on any requested GPU;
   - take the minimum continuous end across all requested GPUs.
8. If `max_contiguous_hours < duration`, stop before creating reservations and
   report failure details: node, requested GPUs, requested duration, existing
   coverage, max continuously reservable hours, and the first blocker per GPU
   when visible.
9. If existing reservations continuously cover every requested GPU through
   `target_expires_at`, do not create a new reservation.
10. Otherwise create only missing reservation windows. Split each missing window
    into chunks of at most 24 hours. Group GPUs with the same missing chunk
    window into one MCP reservation when possible:

```text
create_reservation(server_id, gpus=[...], purpose, starts_at, expires_at, mode="reserved")
```

For an immediate chunk of 24 hours or less, `ttl` may be used as `<hours>h`.
For chained chunks, extensions, or future starts, use explicit `starts_at` and
`expires_at`.

11. If a chunk creation fails despite preflight, stop and report failed protect:
    created chunks if any, missing chunks, max continuously reservable hours
    from the latest verified snapshot if available, and the tool error. Do not
    revoke created chunks unless the user explicitly asks for cleanup.
12. Ensure authorization through MCP. Avoid duplicates when an active rule
    already covers the scope:

```text
allow(server_id, mode="user", user="<linux-user>")
allow(server_id, mode="docker", container="<container>")
allow(server_id, mode="k8s", namespace="<namespace>")
```

13. Verify with `fleet_snapshot` or `status` and report non-secret metadata:
    node, GPUs, purpose, requested duration, reused coverage, chunks created,
    authorization scope, and IDs returned by tools.

If GPUardian MCP tools are not available, say that reservation/protect requires
the GPUardian MCP connection. Do not fall back to raw web API calls or root-key
CLI registration unless the user explicitly asks.

## Authorize-Only and CLI Workflow

Use this when the user asks only to authorize/allow an existing reservation or
when MCP is unavailable but a fixed key is available.

Inspect first:

```bash
KEY=gk_xxx gpuardian token info
KEY=gk_xxx gpuardian ps
```

Then authorize exact scopes:

```bash
KEY=gk_xxx gpuardian allow user --name "$(id -un)"
KEY=gk_xxx gpuardian allow docker --container <container-name-or-id>
KEY=gk_xxx gpuardian allow k8s --namespace <namespace>
```

For a bare command that Codex is actually launching, use:

```bash
KEY=gk_xxx gpuardian run -- <command>
```

Do not use `gpuardian run -- docker run ...`; Docker runs the real workload in
a different cgroup. Protect Docker workloads with `gpuardian allow docker`.

## Yield Workflow

Use this when the user wants another person/session to use the GPU without
revoking the scheduled reservation or rotating the fixed key.

1. Inspect with `fleet_snapshot` when MCP is available, otherwise use:

```bash
KEY=gk_xxx gpuardian token info
KEY=gk_xxx gpuardian ps
```

2. Identify the recipient exact scope. If missing or ambiguous, ask only for
   `user:<name>`, `docker:<container>`, or `k8s:<namespace>`.
3. Add the recipient authorization with MCP `allow` or CLI `gpuardian allow`.
4. Do not revoke the reservation or key. Leave old authorizations alone unless
   the user explicitly asks for admin cleanup.

## Account and Key Notes

- Users get GPUardian access through the web gateway: create/sign in, reserve
  GPUs in Schedule or via MCP, then use the fixed key from the Key tab for
  node-side CLI workflows.
- Each account has one fixed `gk_...` key shared across synced nodes and
  reservations. Reserving another window does not change the key.
- Revoking a reservation does not rotate or invalidate the fixed key.
- GPUardian supports AMD and NVIDIA nodes.
- Kubernetes authorization is namespace-level, not pod-level.
- Wildcards can authorize more than intended; treat them as admin-only.

## Failure Handling

- If required protect fields are missing, report `GPU`, `duration`, and/or
  `purpose` as missing and stop.
- If setup fields are missing, ask only for `username` and/or `password`.
- If requested continuous coverage cannot be satisfied, report failed protect
  with max continuously reservable hours and visible blockers.
- If MCP is unavailable for protect/reserve, ask the user to connect/configure
  GPUardian MCP with gateway URL and account credentials.
- If current node cannot be resolved uniquely, ask for `node`.
- If a recipient authorization is ambiguous, ask for the exact typed scope.
- If a tool returns a secret, redact it before responding.

## Response Pattern

Keep responses short:

- On setup success, report configured username and ask the user to restart Codex
  before first use.
- Say whether the action was protect/reserve, authorize-only, yield, reclaim, or
  inspect.
- On success, report node, GPUs, purpose/window, requested duration, reused
  coverage, chunks created, authorization scope, and relevant IDs.
- On failed protect, report node, GPUs, requested duration, max continuously
  reservable hours, existing coverage, and first visible blocker details.
- Mention if old rules were intentionally left alone.
- Never include raw keys, passwords, tokens, cookies, or secrets.
