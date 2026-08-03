---
name: user-cli-ops
description: How an end-user runs GPU workloads and manages authorizations with the gpuardian CLI using their fixed gk_ key — run a job, allow a docker/k8s/user scope, check status, list processes, and inspect a token. Use whenever a user asks to run a job or training, allow a container or namespace, check status, see what's running, or use gpuardian run/allow/status/ps/token — even if they just say "launch a job" or "is my GPU free".
---

# Run workloads and manage authorizations with the CLI

The `gpuardian` CLI authenticates to the node daemon over a Unix socket using
your **fixed key** (`KEY=gk_...`). A regular user never needs the root key
(`ROOT_KEY=rk_...`) — that's admin-only.

## Credentials and socket

```bash
export KEY=gk_xxx                       # your fixed key (from the web Key tab)
export GPUARDIAN_SOCKET=/run/gpuardian.sock   # default; override if the node differs
```

The CLI dials `GPUARDIAN_SOCKET` (default `/run/gpuardian.sock`) with a 5s
connect / 30s deadline. On a node where the socket path differs, prefix
commands with `GPUARDIAN_SOCKET=/path/to/sock`.

## Run a workload

```bash
KEY=gk_xxx gpuardian run -- python train.py
KEY=gk_xxx gpuardian run -- torchrun --nproc_per_node=8 train.py
KEY=gk_xxx gpuardian run --name "GLM TP4 benchmark" -- python train.py
```

Everything after `--` is the workload command. Child processes inherit the
authorization. The CLI resolves the command via the caller's `PATH` and sends
`Command`, `Workdir` (cwd), and `Env` (full `os.Environ()`) over the socket.
For a claimed-mode key, optional `--name` labels the run in Dashboard → GPU
Activity.

## Authorize a container / k8s / user (instead of `run -- docker`)

**`gpuardian run -- docker run ...` is unsupported** — Docker puts the
workload in a different cgroup, so GPUardian can't track or enforce it.
Authorize the scope instead:

```bash
KEY=gk_xxx gpuardian allow docker --container trainer --run-name "GLM TP4 benchmark"
KEY=gk_xxx gpuardian allow k8s   --namespace training --run-name "GLM TP4 benchmark"
KEY=gk_xxx gpuardian allow user  --name alice --run-name "GLM TP4 benchmark"
```

`--run-name` is optional; omit it to keep the default `Claimed run` label.
Docker rules may use a future exact name or a wildcard and are stored even
before a matching container exists. Use the **narrowest scope**. Wildcard K8s
namespaces and usernames remain admin-only.

## Inspect status and keys

```bash
KEY=gk_xxx gpuardian status      # JSON; filters out revoked/expired tokens,
                                 # reservations, authorizations, bypasses, soft-claims
KEY=gk_xxx gpuardian ps          # tabular: id gpu user command
KEY=gk_xxx gpuardian token info  # info about the current token
```

`status` and `ps` use `tokenFromEnv()` (optional KEY; the usage text notes
"root may omit KEY"). `token info` requires KEY.

## Gotchas

- **`run -- docker run ...` is unsupported.** Docker places the workload in a
  different cgroup; authorize the container with `allow docker --container`
  instead. Keep this invariant in mind for any enforcement-related change.
- **One fixed key per account**, shared across all nodes and reservations.
  Revoking a reservation does **not** rotate the key; use `Regenerate` in the
  web UI (or the `regenerate_key` MCP tool) to replace it.
- **The CLI is the node-side tool.** It talks to the daemon on the node where
  the GPUs are — it does not talk to the web gateway. Run it on the GPU node
  (or a host that can reach the node's socket).

## Read before sensitive edits

- `README.md` — CLI reference (subcommands, flags, env vars).
- `cmd/gpuardian/main.go` — subcommand dispatch and the KEY-vs-ROOT_KEY split.
- `AGENTS.md` — "`gpuardian run -- docker run ...` is unsupported" gotcha.
