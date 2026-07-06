# Rocguard

Rocguard is a local AMD GPU guard for shared Linux servers. It provides a root
daemon plus a small CLI wrapper so users can run GPU workloads only through an
active lease.

The v1 enforcement model is intentionally simple:

- `rocguardd` monitors AMD GPU process ownership with `amd-smi process --json`.
- A GPU is enforced only while it has an active Rocguard lease.
- If a GPU has no active Rocguard lease, Rocguard ignores that GPU and never
  kills existing workloads there.
- While a GPU is leased, processes that do not match the lease or a bypass rule
  are terminated.

This is monitor-kill enforcement, not hard device isolation. Users with root,
sudo, or root-equivalent Docker access can bypass it.

## Requirements

- Linux with cgroup v2.
- AMD ROCm tooling with `amd-smi`.
- Go 1.22+ to build from source.
- Root access to run the daemon.
- Optional: `docker`, `crictl`, or `kubectl` for Docker/Kubernetes modes.

## Build

```bash
go build -buildvcs=false -o rocguard ./cmd/rocguard
```

The `-buildvcs=false` flag is useful in restricted worktrees where Git metadata
may not be fully available.

## Quick Start

Start the daemon as root:

```bash
sudo ./rocguard daemon
```

In another terminal, create or show the local root key:

```bash
sudo ./rocguard show-key
```

Register a user token:

```bash
./rocguard register
```

The command prompts for:

```text
Root key:
Name:
TTL [2h]:
```

Use the returned token to run a GPU command:

```bash
KEY=rg_xxx ./rocguard run --gpu 0 -- python train.py
```

The `--gpu` flag tells the daemon which host GPU to lease and monitor. Rocguard
does not set `HIP_VISIBLE_DEVICES`, `ROCR_VISIBLE_DEVICES`, or similar GPU
visibility variables for the wrapped command.

Check current state:

```bash
./rocguard status
./rocguard ps
./rocguard who --gpu 0
KEY=rg_xxx ./rocguard token info
```

## Docker Mode

Authorize a specific Docker container for a GPU:

```bash
KEY=rg_xxx ./rocguard docker allow --gpu 0 --container trainer
```

Rocguard resolves the container name to an immutable container ID at lease
creation time. The mutable container name is not trusted during enforcement.

For this to be meaningful, regular users should not have direct access to the
Docker socket. Membership in the `docker` group is effectively root-equivalent.

## Kubernetes Mode

Authorize a Kubernetes namespace for a GPU:

```bash
KEY=rg_xxx ./rocguard k8s allow --gpu 0 --namespace training
```

Rocguard maps GPU PIDs to container IDs and then to Kubernetes namespaces using
`crictl inspect` first, with `kubectl get pod -A -o json` as a fallback. If
neither runtime path is available, Kubernetes lease creation fails.

Namespace-level authorization is broad: any pod in that namespace can match the
lease for the selected GPU.

## Bypass Rules

Bypass rules are intended for trusted host agents such as GPU metrics daemons.

Bypass one PID:

```bash
sudo ./rocguard bypass add --pid 1234 --ttl 24h --reason gpuagent
```

Bypass a command path for a specific UID:

```bash
sudo ./rocguard bypass add --command /usr/bin/gpuagent --uid 0 --ttl 24h --reason gpuagent
```

Bypasses expire automatically when their TTL ends.

## Revoke

Revoke a token, lease, or bypass ID:

```bash
sudo ./rocguard revoke <token-or-lease-id>
```

Revoking a lease stops enforcement for that GPU after the lease is marked
inactive.

## Runtime Paths

Defaults:

```text
/run/rocguard.sock
/var/lib/rocguard/state.json
/var/lib/rocguard/root.key
/var/log/rocguard/audit.log
/sys/fs/cgroup/rocguard/lease_<id>
```

Environment overrides:

```text
ROCGUARD_SOCKET
ROCGUARD_STATE
ROCGUARD_ROOT_KEY
ROCGUARD_AUDIT_LOG
ROCGUARD_CGROUP_ROOT
ROCGUARD_PROC_ROOT
```

These are useful for local testing without writing to `/var` or `/run`.

## Local Development

Run tests:

```bash
GOCACHE=/tmp/rocguard-go-build go test ./...
```

Build:

```bash
GOCACHE=/tmp/rocguard-go-build go build -buildvcs=false -o rocguard ./cmd/rocguard
```

Run a non-root smoke test for root-key creation with temporary paths:

```bash
ROCGUARD_ROOT_KEY=/tmp/rocguard/root.key \
ROCGUARD_STATE=/tmp/rocguard/state.json \
ROCGUARD_AUDIT_LOG=/tmp/rocguard/audit.log \
./rocguard show-key
```

## Safety Notes

Do not test Rocguard on a production GPU with active workloads unless you are
ready for unauthorized processes on a leased GPU to be killed.

The safest first test is:

1. Pick an idle GPU.
2. Start `sudo ./rocguard daemon`.
3. Register a short-lived token.
4. Run one known command with `KEY=... ./rocguard run --gpu <id> -- ...`.
5. Confirm `./rocguard ps` and audit output.

Rocguard does not currently configure Linux device permissions, ROCm device
ACLs, or container runtime isolation. It detects and kills unauthorized GPU
users after they appear in AMD SMI.
