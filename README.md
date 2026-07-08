# Rocguard

Rocguard is a local AMD GPU guard for shared Linux servers. It provides a root
daemon plus a small CLI wrapper so users can run GPU workloads through
root-authorized reserved or claimed-mode keys.

The enforcement model is intentionally simple:

- `rocguardd` monitors AMD GPU process ownership with `amd-smi process --json`.
- Rocguard only treats a PID as active GPU usage when AMD SMI reports non-zero
  GPU memory for that PID. Parent launcher processes that do not hold GPU memory
  are ignored and are not killed.
- `reserved` registration reserves a GPU before use. While the reservation is
  active, non-bypassed processes on that GPU must match an authorization for
  the reservation token.
- `claimed` registration creates a non-expiring key. A claimed-mode
  authorization claims a GPU only after an authorized process is observed on an
  otherwise clean GPU.
- If a claimed-mode process appears on a GPU that already has unrelated
  processes, Rocguard leaves the GPU unclaimed and does not kill those existing
  workloads.

This is monitor-kill enforcement, not kernel device isolation. Users with root,
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

In another terminal, get the root key from the root-owned key file. By default
that file is `/var/lib/rocguard/root.key`; if `ROCGUARD_ROOT_KEY` is set, use
that path instead. There is intentionally no Rocguard CLI command that prints
the root key.

Register a claimed-mode key:

```bash
./rocguard register --claimed
```

The command prompts for:

```text
Root key:
Name:
```

Or reserve one or more GPUs with a reserved key:

```bash
./rocguard register --reserved
```

Reserved registration prompts for:

```text
Root key:
Name:
GPUs:
TTL [2h]:
```

Use a comma-separated list such as `0,1` to reserve more than one GPU with the
same token and TTL.

Reserved TTL is capped at 8 hours.
Only `--reserved` and `--claimed` are valid registration modes.

Use the returned token to run a GPU command:

```bash
KEY=rg_xxx ./rocguard run -- python train.py
```

Rocguard does not set `HIP_VISIBLE_DEVICES`, `ROCR_VISIBLE_DEVICES`, or similar
GPU visibility variables for the wrapped command.

Check current state:

```bash
./rocguard status
./rocguard ps
KEY=rg_xxx ./rocguard token info
ROOT_KEY=rk_xxx ./rocguard show-keys
```

`rocguard ps` prints a table with `id`, `gpu`, `user`, and `command`. Idle
reserved GPU reservations appear as `reserved until <timestamp>`.

Admin commands that need the root key read it from `ROOT_KEY` or prompt for it.

`allow` scope values support `*` as a wildcard. For example, `codex*` matches
`codex`, `codex-1`, and `codex-worker`.

## Command Reference

```text
rocguard help
rocguard daemon [--dry-run]
rocguard register (--reserved | --claimed)
KEY=... rocguard run -- <command>
KEY=... rocguard allow docker --container <name-or-id>
KEY=... rocguard allow k8s --namespace <name>
KEY=... rocguard allow user --user <name>
rocguard status
rocguard ps
KEY=... rocguard token info
ROOT_KEY=... rocguard show-keys
ROOT_KEY=... rocguard bypass add (--pid <pid> | --command <path> --uid <uid>) --ttl <duration> --reason <text>
ROOT_KEY=... rocguard revoke <token-or-reservation-or-authorization-or-bypass-id>
```

## Docker Mode

Authorize a specific Docker container:

```bash
KEY=rg_xxx ./rocguard allow docker --container trainer
```

For exact container names, Rocguard resolves the container name to an immutable
container ID at authorization time. The mutable container name is not trusted
during enforcement. Wildcard container values such as `codex*` are matched
dynamically against Docker container names during enforcement.

For this to be meaningful, regular users should not have direct access to the
Docker socket. Membership in the `docker` group is effectively root-equivalent.

## Kubernetes Mode

Authorize a Kubernetes namespace:

```bash
KEY=rg_xxx ./rocguard allow k8s --namespace training
```

Rocguard maps GPU PIDs to container IDs and then to Kubernetes namespaces using
`crictl inspect` first, with `kubectl get pod -A -o json` as a fallback.
Namespace values support wildcards such as `training-*`.

Namespace-level authorization is broad: any pod in that namespace can match the
authorization.

## User Mode

Authorize all processes for one Linux user:

```bash
KEY=rg_xxx ./rocguard allow user --user alice
```

User values support wildcards such as `codex*`.

## Bypass Rules

Bypass rules are intended for trusted host agents such as GPU metrics daemons.

Bypass one PID:

```bash
ROOT_KEY=rk_xxx ./rocguard bypass add --pid 1234 --ttl 24h --reason gpuagent
```

Bypass a command path for a specific UID:

```bash
ROOT_KEY=rk_xxx ./rocguard bypass add --command /usr/bin/gpuagent --uid 0 --ttl 24h --reason gpuagent
```

Bypasses expire automatically when their TTL ends.

## Revoke

Revoke a token, reservation, authorization, or bypass ID:

```bash
ROOT_KEY=rk_xxx ./rocguard revoke <id>
```

Revoked objects are deleted from state and no longer appear in `status` or
`show-keys`. Revoking a token also deletes reservations, authorizations, and
claimed GPUs created by that token.

## Runtime Paths

Defaults:

```text
/run/rocguard.sock
/var/lib/rocguard/state.json
/var/lib/rocguard/root.key
/var/log/rocguard/audit.log
/sys/fs/cgroup/rocguard/auth_<id>
```

Environment overrides:

```text
ROCGUARD_SOCKET
ROCGUARD_STATE
ROCGUARD_ROOT_KEY
ROCGUARD_AUDIT_LOG
ROCGUARD_CGROUP_ROOT
ROCGUARD_PROC_ROOT
ROCGUARD_DRY_RUN
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

Run a root-key smoke test with temporary paths by provisioning the key file
directly:

```bash
mkdir -p /tmp/rocguard
printf 'rk_dev_only\n' > /tmp/rocguard/root.key
chmod 600 /tmp/rocguard/root.key
ROCGUARD_ROOT_KEY=/tmp/rocguard/root.key \
ROCGUARD_STATE=/tmp/rocguard/state.json \
ROCGUARD_AUDIT_LOG=/tmp/rocguard/audit.log \
ROOT_KEY=rk_dev_only ./rocguard show-keys
```

Run light bare-metal integration tests:

```bash
KEY=rg_xxx ROOT_KEY=rk_xxx ./scripts/integration_test.py --gpus 2,3
```

The integration runner:

- auto-bypasses pre-existing AMD SMI PIDs on the selected GPUs;
- uses small allocations and sleeps between compute iterations;
- tests multi-GPU `hold_gpu.py`;
- tests child GPU processes with `hold_gpu.py --children` staying authorized
  inside the Rocguard cgroup.

Optional Docker test, using a ROCm/PyTorch image that already has Python and
Torch:

```bash
KEY=rg_xxx ROOT_KEY=rk_xxx ./scripts/integration_test.py \
  --gpus 2 \
  --docker-image <rocm-pytorch-image>
```

Optional Kubernetes test:

```bash
KEY=rg_xxx ROOT_KEY=rk_xxx ./scripts/integration_test.py \
  --gpus 2 \
  --k8s-namespace training \
  --k8s-image <rocm-pytorch-image> \
  --k8s-gpu-resource amd.com/gpu
```

The script needs `ROOT_KEY` for auto-bypass setup and cleanup revoke calls.
Pass `--no-auto-bypass` only when you have already confirmed the selected GPUs
have no unrelated workloads or you intentionally want to skip that safety step.

## Safety Notes

Do not test Rocguard on a production GPU with active workloads unless you are
ready for unauthorized processes on a reserved or claimed GPU to be killed.

The safest first test is:

1. Pick an idle GPU.
2. Start the daemon in dry-run mode with temporary paths:

   ```bash
   sudo env \
     ROCGUARD_SOCKET=/tmp/rocguard.sock \
     ROCGUARD_STATE=/tmp/rocguard/state.json \
     ROCGUARD_ROOT_KEY=/tmp/rocguard/root.key \
     ROCGUARD_AUDIT_LOG=/tmp/rocguard/audit.log \
     ROCGUARD_CGROUP_ROOT=/tmp/rocguard/cgroup \
     ./rocguard daemon --dry-run
   ```

3. Register a short-lived reserved token for that idle GPU.
4. Run one known command with `KEY=... ./rocguard run -- ...`.
5. Confirm `./rocguard ps` and audit output.
6. Restart the daemon without `--dry-run` only after the dry-run decisions look
   correct.

Rocguard does not currently configure Linux device permissions, ROCm device
ACLs, or container runtime isolation. It detects and kills unauthorized GPU
users after they appear in AMD SMI.
