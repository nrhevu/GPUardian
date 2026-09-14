# AMD process collection hotfix — 2026-09-14

## Change

GPUardian now enumerates AMD GPU indices with `amd-smi list --json` and
queries each GPU with a separate `amd-smi process --gpu INDEX --json`
invocation. This avoids the incorrect per-process memory values observed
when AMD SMI 26.2.2 queried all GPUs in one invocation on Alpha.

At most four process queries run concurrently, sharing the existing five
second provider deadline. GPU indices are validated, including sparse indices,
and each response must contain exactly the requested GPU. Any failed or invalid
GPU response fails the complete sample. The collector does not fall back to the
known-bad bulk result or report a partial sample as an empty process inventory.
The zero-memory filter and authorization/enforcement rules are unchanged.

## Validation

- Regression reproduces PID 2828560 having zero memory in bulk output but
  270,846,402,560 bytes on GPU 1 when queried separately.
- Tests cover correct GPU attribution, idle GPUs, invalid/partial responses,
  duplicate/invalid indices and a shared deadline.
- `go test -race ./internal/amdsmi`: passed.
- Linux `go test ./...` and `go vet ./...`: passed with Go 1.26.5.
- The new collector was also run directly on Alpha without starting another
  daemon. Authorization decisions were simulated with `DryRun=true` and no
  state writes or process signals. A sample of eight GPUs took about 518 ms.

## Alpha rollout

Host: `168.144.61.194`, SSH alias `onenexus-alpha-mi355x8-root`.
Source checkout: `/root/gpuardian`, base revision `35f2e2dd4b61`.

Updated both the source checkout and binaries at `/usr/local/bin/gpuardian`
and `/root/gpuardian/gpuardian`. The source includes the earlier History v9
fix as well as this AMD collector fix. The initial Alpha rollout used
uncommitted working-tree changes; subsequent publication is recorded in Git.

Binary: Go 1.26.5, Linux/amd64, CGO disabled.
SHA-256: `b559e0dc124a18f97e4eae8ceb4081aa02e13817b1d812299da7ff21ff92d34a`.
Build/source hashes are saved in `.dev/node-hotfix/build.json` locally and
`/root/gpuardian-node-hotfix-20260914/build.json` on Alpha.

Before restart, the systemd service cgroup contained only the daemon PID.
The previous binary, changed source files and stopped node state were backed
up under `/root/gpuardian-node-hotfix-20260914/before/`. The service was
stopped, the state archive taken, and the binary replaced atomically before
starting it again. The running process hash was checked against the build.
The gateway Deployment was not changed by this node rollout.

## Live end-to-end test

A dedicated container named `gpuardian-process-check-20260914` allocated
64 MiB on an available GPU 0, using a narrowly scoped Docker authorization
belonging to admin. The cached ROCm image was used with no network access,
read-only root filesystem, limited host memory and process count, and
`HIP_VISIBLE_DEVICES=0` / `ROCR_VISIBLE_DEVICES=0`.

The first test attempt could not initialize HIP with only one render device
mounted and was fully cleaned up. The successful attempt used the standard
ROCm `/dev/kfd` and `/dev/dri` mounts while retaining the GPU visibility limits.

- PID: `140704`; authorization: `auth_ec28fe0281e39ad8`.
- The new collector saw exactly 67,108,864 bytes on GPU 0 for that PID.
- The dry-run authorizer returned `allow`, holder `admin`, reason `claimed`.
- After rollout, `gpuardian ps` showed the process.
- Production History recorded job `exec_c7da964dd0c4e64995828b3e`, under
  claimed session `sess_3e799b87a554aa36b5172745`, initially active.
- After stopping/removing the container and revoking its authorization,
  both the job finish and session finalization were set to
  **2026-09-14 12:32:24 UTC (19:32:24 Vietnam time)**, reason `process_gone`.
- At 12:33:08 UTC the gateway's Alpha cursor was 87338, synced at 12:33:06.

The completed admin test record is intentionally retained in History under
`GPUardian process collector verification (64 MiB)`. Test containers and
authorizations were cleaned up; the temporary read-only Kubernetes reader
pod was removed after verification.

## Subsequent deployment

Alpha already runs the fix. For another AMD node, deploy a binary built from
the source containing `internal/amdsmi/processes.go` and the matching change
to `amdsmi.go`; installing the old Git revision alone will lose the fix.
Only Alpha was updated and live-tested in this task. Build and installation
instructions are in the repository README. Preserve node identity, keys,
state and telemetry, and ensure a service restart will not stop workloads
inside its cgroup before replacing the daemon.

For a binary rollback on Alpha, stop `gpuardian`, restore
`before/gpuardian` to `/usr/local/bin/gpuardian` and `before/repo-gpuardian`
to `/root/gpuardian/gpuardian`, then start the service. This node fix does
not migrate the node JSON state; restoring old node state is unnecessary
and would discard newer activity. Rolling back the gateway is a separate
operation governed by its History schema v9 restore contract.
