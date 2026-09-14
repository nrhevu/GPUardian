# Claimed History hotfix — 2026-09-14

The fix recomputes a claimed session from all its `job_sessions` links when
any linked job changes, even after the job's primary session has moved to a
reservation. A remaining unfinished sibling keeps the claim active; completed
jobs finalize the claim using their stored finish timestamps.

Start and end now come from the same aggregate, with end at least one
millisecond after start. Claimed inserts also apply that bound after converting
timestamps to milliseconds, preventing submillisecond windows from blocking
ingestion.

Migration **v9** repairs existing claimed aggregates using the same job-link
semantics. It leaves claims without job evidence alone, preserves session/job
rows and invalidates cached daily summaries. Previous migrations are unchanged.
The repair SQL is stored as an independent migration literal so later runtime
changes cannot change an already-applied migration checksum.

## Verification

Regression tests reproduce the original failures and cover:

- A claimed job moving to a reservation and finishing.
- An unfinished sibling keeping the claim active.
- A new job reusing an authorization after previous jobs moved away, including
  cursor advancement and duplicate page ingestion.
- A submillisecond initial window.
- Migration of stale records while preserving live and evidence-free claims.

The migration was also exercised on a SQLite Online Backup of the production
database, in the diagnostic pod's `/tmp` while the original PVC was read-only:

| Check | Before | After |
| --- | ---: | ---: |
| Incorrectly active claimed sessions with all linked jobs finished | 21 | 0 |
| Sessions | 832 | 832 |
| Jobs | 6,482 | 6,482 |
| Job/session links | 6,892 | 6,892 |

SQLite integrity was `ok` before and after the rehearsal. This rehearsal copy
is not the rollback backup.

Passed `go test ./...` and `go vet ./...` in Linux using Go 1.26.5,
`go test -race ./internal/history`, and a Linux/amd64 cross-build and vet.
The saved Helm chart passes lint; its rendered resources have no diff against
the cluster after the image update.

## Production rollout and verification

The Deployment successfully rolled out the image below. At
2026-09-14 11:07:26 UTC (18:07 Vietnam time):

- Gateway pod `gpuardian-b655c6656-d999h` was Ready, with zero restarts.
- Schema version was 9; incorrectly active claims with all linked jobs
  finished had fallen from 21 to 0.
- Alpha's cursor advanced from 76,720 to 86,149, with its most recent
  successful sync at 11:07:24 UTC. The backlog was being consumed again.
- Alpha had 196 stored jobs, including a job started at 10:24:04 UTC.
- The public HTTPS endpoint returned HTTP 200.
- The browser confirmed that the previously stuck 05:16 RC1 GLM claim now
  showed `completed`, with two jobs and an end at 05:20 Vietnam time.
  Newer records and utilization through 18:11 were visible.

Before migration, the gateway was stopped briefly and the complete state
directory was archived. The verified local rollback backup is
`.dev/history-hotfix/state-before-history-fix.tar.gz` (27,847,761 bytes,
mode 0600, excluded from Git). SHA-256:
`44795ddf72f04eb748f4fae98384e7face93e828bed8c8f8b4ac193aeb8ff44e`.

## Remaining live-process visibility issue

**Update:** subsequently fixed and deployed on Alpha; see
[the node collector hotfix and live lifecycle test](NODE-PROCESS-FIX-2026-09-14.md).
The observations below describe the state before that node fix.

The two History ingestion defects are fixed, but a current Alpha reservation
still showed zero observed jobs despite high GPU utilization. At about
11:13 UTC, read-only SSH checks on the registered host `168.144.61.194`
found no active bypasses and `gpuardian ps` listed reservations only.
`amd-smi process --json` returned 13 live PIDs on every GPU, with legacy
`mem_usage.value=0`; none had per-process VRAM above 100 MB. These PIDs
also appeared under `/sys/class/kfd/kfd/proc`.

The installed tool reports AMD SMI 26.2.2, ROCm 7.2.4 and amdgpu 6.16.13.
The daemon parser reads `mem_usage`; enforcement skips known-zero memory
processes before job tracking. Thus fresh gateway ingestion does not itself
recover these missing observed-job records. The discrepancy between device
utilization and process counters was initially unresolved. The subsequent SSH
check below isolates it to multi-GPU process reporting. This hotfix does not
change daemon enforcement or restart the GPU node. Do not treat zero observed
jobs as zero GPU activity.

### SSH follow-up: multi-GPU process query, approximately 19:00 Vietnam time

On Alpha, PID **2828560** (`sglang::schedul`) belongs to the running container
`sglang-issue2-rc2-dsv4-bf16-20260914t094000z`. It has an active Docker
authorization owned by `longluong2`, and GPU 1 has an active reservation.

The same PID produced these results in a bulk → single → bulk comparison:

| Read-only source | GPU 1 process memory |
| --- | ---: |
| `amd-smi process --json` | 0 bytes |
| `amd-smi process --gpu 1 --json` | 270,846,402,560 bytes (~252.25 GiB) |
| Repeat `amd-smi process --json` | 0 bytes |
| `/sys/class/kfd/kfd/proc/2828560/vram_49577` | 270,846,402,560 bytes |

`amd-smi list --json` maps KFD GPU ID 49577 to GPU 1, PCI 0000:8b:00.0.
The matching `/proc/2828560/fdinfo/9` also reports substantial VRAM
(`drm-total-vram: 263434584 KiB`). No job event for PID 2828560 was found
in the retained telemetry outbox. The outbox itself continued to emit GPU
samples (sequence 86809 at 12:01:02 UTC).

GPUardian's `internal/amdsmi/amdsmi.go` calls the bulk command. Its parser
accepts a numeric zero as known zero memory. `usesGPUResources` in
`internal/enforce/enforce.go` then returns false; the decision is `skip`,
so `trackObservedTelemetryJobs` never emits `job.started` for this process.
This establishes the missing-job path for this concrete process without
assuming that the workload stopped or that AMD SMI was missing. The precise
upstream implementation defect inside the bulk AMD SMI call was not traced.

Separately, an authorized job using its owner's active reservation belongs
under that reservation; it need not create a separate `Claimed` activity.
The remaining fix concerns reliable process collection on the node. No
daemon, GPU workload, driver, reservation or authorization was changed during
this follow-up.

## Image

- Base source revision: `35f2e2dd4b61` plus the local History patch.
- Compiler: Go 1.26.5, `CGO_ENABLED=0`, target `linux/amd64`.
- Image: `ghcr.io/nrhevu/gpuardian:35f2e2dd4b61-history-20260914`.
- Image digest: `sha256:bee479c0f7ffa96ae8705af35dc333eebf4fd0c9fb9c69e37bedcbd10341474d`.
- Reuses the deployed UI and distroless runtime from base digest
  `sha256:9f274ee82e423ecb5edf55b2dd1ce8cd6e985792fdfa42a4abd06c6566cd37fa`;
  replaces `/usr/local/bin/gpuardian` with the rebuilt binary.
- Exact file and binary hashes are recorded locally in the protected
  `.dev/history-hotfix/build.json`.

## Rollback contract

The old binary rejects schema v9. Do not roll back only the Deployment image.
Stop the gateway, restore the complete pre-upgrade state backup (including
the matching users/key files and database/WAL if present), then restart the
previous image. Follow the state ownership and SQLite rules in
[the restore runbook](../deploy/RESTORE.md).

The initial image deployment used working-tree changes. Subsequent source
publication is recorded in Git independently of the deployed image tag.
