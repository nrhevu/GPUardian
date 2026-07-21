# AGENTS.md

Workspace instructions for ZCode agents working in this repository.
Read this before editing. Project setup and operator workflows live in
`README.md` (production) and `DEVELOPMENT.md` (local no-TLS dev); this file
captures only things future agents would otherwise miss.

## What this repo is

GPUardian reserves and enforces access to AMD and NVIDIA GPUs on shared Linux
servers. It is **monitor-and-kill enforcement**, not kernel-level device
isolation — a user with root, sudo, or root-equivalent Docker access can
bypass it.

Three components share a single Go module (`module gpuardian`, Go 1.25):

1. **Node daemon** (`gpuardian daemon`) — runs on every AMD or NVIDIA GPU node
   under systemd. Reads `/proc`, uses the vendor's SMI tooling (`amd-smi` or
   `nvidia-smi`, selected via `GPUARDIAN_GPU_VENDOR`), manages cgroups,
   launches workloads. Must run on the host, never in a container. Port
   `8192` (HTTPS in prod, `8193` HTTP in dev with `--dry-run`).
2. **CLI** (`cmd/gpuardian`) — `run`, `allow`, `status`, `ps`, `register`,
   `token info`, `show-keys`, `bypass`, `revoke`. Single binary shared by
   daemon and CLI; subcommand dispatch is in `cmd/gpuardian/main.go`.
3. **Web gateway** (`internal/web`) — Dockerized, non-root (UID/GID `65532`),
   serves accounts, scheduling, keys, fleet of nodes. Port `8443` (prod) or
   loopback `18080` (dev). Frontend is a React 19 + Vite SPA in `web/ui/`.

## Layout

```
cmd/gpuardian/         CLI + daemon entrypoint (single main.go)
internal/protocol/    JSON Request/Response wire format shared by daemon <-> gateway <-> CLI
internal/daemon/      Node daemon: HTTP server, snapshot, telemetry, cgroup/enforce entry
internal/enforce/     Monitor-and-kill enforcement logic
internal/store/       Daemon-side persistent state (state.json)
internal/config/      Env-var config for both daemon and gateway
internal/model/       Shared domain types
internal/web/         Web gateway: HTTP handlers, sessions, users, registry, keys, history
internal/history/     Reservation history SQLite store (history.db)
internal/netlimit/    Rate limiting for gateway endpoints
internal/amdsmi/      amd-smi wrapper (AMD provider)
internal/nvidiasmi/   nvidia-smi wrapper (NVIDIA provider)
internal/gpusmi/      Vendor-neutral GPU provider interfaces shared by daemon
internal/proc/        /proc parsing
internal/runtime/     Workload process runtime
internal/telemetry/   Telemetry outbox
web/ui/               Vite + React 19 SPA (src/main.jsx, src/styles.css — single-file app)
scripts/              Python helpers (integration_test.py, hold_gpu.py) — not part of build
compose.web.yml       Production gateway Compose
compose.web-dev.yml   Dev gateway Compose (loopback, no TLS, --dry-run node)
Dockerfile.web        Hardened non-root gateway image
```

## Build, test, lint

Run from repo root:

```bash
# UI (must build before gateway so embedded assets exist)
npm --prefix web/ui ci
npm --prefix web/ui run build      # outputs web/ui/dist/, embedded via web/gpuardian-ui/

# Go
go test ./...
go build -buildvcs=false -o gpuardian ./cmd/gpuardian

# Focused Go tests
go test ./internal/web/...
go test -run TestName ./internal/web/

# Gateway image
sudo docker compose -f compose.web.yml build        # prod
sudo docker compose -f compose.web-dev.yml up -d --build   # dev
```

There is no separate lint config in the repo; `go vet ./...` and `go test
./...` are the gates. The UI has no test runner configured.

## Architecture boundaries

- **`internal/protocol` is the wire contract.** Both the daemon and gateway
  speak the JSON `protocol.Request`/`protocol.Response` envelope over the
  node API. Changes here are cross-component and must stay backward-compatible
  with deployed daemons.
- **Daemon (`internal/daemon`, `internal/enforce`) must stay host-only.** It
  reads `/proc`, `/sys/fs/cgroup`, and the GPU vendor's SMI CLI (`amd-smi` or
  `nvidia-smi`, selected in `internal/gpusmi` and wired in
  `daemon.selectGPUProvider`). Never add container-only assumptions or imports
  that pull gateway/Docker concerns in.
- **`internal/gpusmi` is the vendor-neutral provider seam.** The daemon holds
  the GPU provider as `gpusmi.Provider` (with optional `gpusmi.MetricsProvider`
  and `gpusmi.DeviceProvider` type assertions). Vendor-specific parsing lives
  only in `internal/amdsmi` and `internal/nvidiasmi`; both satisfy the
  `gpusmi` interfaces via structural typing. Do not import either vendor
  package into the gateway or CLI.
- **Gateway (`internal/web`) talks to nodes only through `NodeAPI`
  (`client.go`).** Do not import daemon packages into the gateway; route
  through the protocol client.
- **UI is a single-file SPA** (`web/ui/src/main.jsx`). Do not introduce a
  framework, router, or build tooling beyond Vite + React 19 without explicit
  intent. Build output in `web/ui/dist/` is what gets served/embedded.
- **`internal/history` is SQLite-backed** via `modernc.org/sqlite` (pure Go,
  no CGO). Do not switch to a CGO sqlite driver — the gateway image is
  non-root and CGO-free.

## Conventions

- **Module path is `gpuardian`** (not `github.com/...`). Internal imports look
  like `gpuardian/internal/web`. Keep this style in new code.
- **Config is env-var driven.** All tunables live behind `GPUARDIAN_*`
  environment variables consumed in `internal/config`. Do not introduce flag-
  based config for the daemon or gateway; add env vars there instead. The CLI
  uses `flag` for subcommand args only.
- **`KEY=rg_...`** is the user-facing authorization credential;
  **`ROOT_KEY=rk_...`** is the per-node admin secret. Never log either. Audit
  log path is `GPUARDIAN_AUDIT_LOG`.
- **All `*_test.go` files live next to their package** (standard Go). Keep
  table-driven test style used throughout `internal/web/*_test.go`.
- **Go files use standard `gofmt`/`goimports` formatting.** No reformatting
  style config exists; match surrounding code.
- **Secrets never go in source control.** `.dev/`, `*.key`, `web.env`,
  `users.json`, `servers.json`, `session.key`, `user-key.key`, `history.db`,
  and the built `gpuardian` binary are all gitignored or operator-only.

## Gotchas

- **Never run two enforcing daemons against the same GPUs.** The dev daemon
  must use `--dry-run` and a separate cgroup root
  (`GPUARDIAN_CGROUP_ROOT=/sys/fs/cgroup/gpuardian-dev`). Production keeps
  `/sys/fs/cgroup/gpuardian`.
- **Dev and prod share `/var/lib/gpuardian-web` as a bind mount path inside
  the container** but dev points the host side at `.dev/web`. Don't confuse
  the two — dev state must stay under `.dev/`.
- **`gpuardian run -- docker run ...` is unsupported.** Docker puts the
  workload in a different cgroup; authorize the container instead with
  `gpuardian allow docker --container <name>`. Keep this invariant in any
  enforcement changes.
- **Command-path bypasses are UID-0 only** (unprivileged mount namespaces can
  spoof executable paths). Prefer PID bypasses. See `bypass` in `main.go`.
- **The gateway image is `read_only: true` with `cap_drop: [ALL]` and
  `no-new-privileges`.** Anything new that needs writable storage must use
  the existing tmpfs `/tmp` or the `/var/lib/gpuardian-web` volume — do not
  add filesystem writes elsewhere without updating the Compose files.
- **`web/ui/dist/` and `web/ui/node_modules/` are gitignored.** Run
  `npm --prefix web/ui ci && npm --prefix web/ui run build` before building
  the gateway image or running Go tests that embed UI assets.
- **`modernc.org/sqlite` requires no CGO**, but its transactions and WAL
  semantics still apply. Back up `history.db` with SQLite Online Backup /
  `VACUUM INTO`, not a live file copy.
- **GPU vendor is per-node, not per-cluster.** `GPUARDIAN_GPU_VENDOR` defaults
  to `auto`, which probes `amd-smi` first then `nvidia-smi`. On a node with
  both installed (rare), set it explicitly. The daemon never opens
  `/dev/nvidia*`, `/dev/kfd`, or `/dev/dri` directly — container device
  passthrough is the caller's responsibility (see `scripts/integration_test.py`
  for the AMD `--device=/dev/kfd --device=/dev/dri` pattern; NVIDIA containers
  pass `--gpus all` or `--device=/dev/nvidia*`).

## Docs to read before sensitive edits

- `README.md` — full production install, CLI reference, env-var list, backup
  contract for gateway state. Read before touching `internal/config`,
  `internal/web/server.go`, or the Compose files.
- `DEVELOPMENT.md` — dev isolation rules. Read before changing dev ports,
  paths, or the `--dry-run` daemon flow.
