# GPUardian Skill User Guide

Use `$gpuardian` when you want Codex to setup, reserve/protect, authorize,
yield, or inspect GPUardian GPU access with minimal typing.

The skill name is **GPUardian**. Legacy `$rocguard` prompts are still accepted.

## Team Setup

Each teammate only needs to tag the skill once. The gateway URL is fixed; they
only paste their GPUardian username and password.

Lazy prompt:

```text
$gpuardian setup
username: your_username
password: your_password
```

Then restart Codex and use `$gpuardian`.

Codex will automatically:

- clone/update `nrhevu/GPUardian`;
- install the GPUardian MCP server in `~/.codex/mcp`;
- write `~/.codex/config.toml` MCP config if missing;
- save credentials to `~/.codex/secrets/gpuardian-mcp.env` with mode `600`;
- avoid storing credentials in the skill folder.

If `username` or `password` is missing, Codex asks only for the missing field.

## Protect

Protect means: ensure exact GPU IDs are reserved continuously for the requested
duration, then authorize one scope.

Required:

- `GPU`: exact GPU IDs.
- `duration`: number of hours.
- `purpose`: why you need the reservation.

Optional:

- `authorize`: defaults to current Linux user.
- `node`: defaults to current GPUardian node.

Lean prompt:

```text
$gpuardian protect
GPU: 0
duration: 2
purpose: dev session
```

Long reservation:

```text
$gpuardian protect
GPU: 0,1
duration: 72
purpose: train benchmark
```

Codex will split this into back-to-back chunks of at most 24 hours. If it cannot
reserve the full 72 hours continuously, the protect request fails and Codex
reports the maximum continuously reservable hours plus conflict details.

Container authorization:

```text
$gpuardian protect
GPU: 2
duration: 3
purpose: docker training
authorize: docker: trainer
```

Kubernetes namespace:

```text
$gpuardian protect
GPU: 0,1
duration: 4
purpose: k8s training
authorize: k8s: training
node: gpu-node-01
```

If `GPU` is missing, Codex stops and asks for exact GPU IDs. It will not
auto-pick GPUs from `1 GPU`, `auto`, or `any`.

If `duration` is missing, Codex stops and asks for duration in hours, for
example `duration: 4`.

If `purpose` is missing, Codex stops and asks for `purpose`.

If you already have a reservation covering the requested GPUs and duration,
Codex does not create another reservation. If your existing reservation is too
short, Codex tries to reserve only the missing tail window. If the tail is more
than 24 hours, Codex splits it into multiple 24h-or-smaller chunks. If it cannot
cover the full requested duration continuously, it reports failure with:

- requested node and GPUs;
- requested duration;
- existing coverage;
- maximum continuously reservable hours;
- first visible conflict/blocker.

## Authorize Only

Use this when a reservation already exists and you only want to add a rule.

```text
$gpuardian authorize using KEY
authorize: current user
```

```text
$gpuardian authorize using KEY
authorize: docker: trainer
```

If no `authorize` is provided, Codex uses the current Linux user.

## Yield

Yield adds someone else's authorization. It does not revoke your reservation,
rotate your key, stop your workload, or delete old rules.

```text
$gpuardian yield
to: user: alice
```

```text
$gpuardian yield
to: docker: trainer
```

```text
$gpuardian yield
to: k8s: training
```

## Reclaim

Use this after yielding when you want your current user/scope authorized again.

```text
$gpuardian reclaim
authorize: current user
```

If you want a new reservation, use `$gpuardian protect` with `GPU`, `duration`,
and `purpose`.

## Inspect

```text
$gpuardian status
```

```text
$gpuardian xem ai đang dùng GPU
```

## Notes

- Protect/reserve requires GPUardian MCP connected to the web gateway.
- First-time setup should be done with `$gpuardian setup`; users normally do not
  run the setup script by hand.
- Node defaults to the current node only when Codex can resolve it uniquely.
- Regular CLI operations use `KEY=gk_xxx`; do not paste secrets into public
  logs or screenshots.
- Docker workloads need `allow docker`, not `gpuardian run -- docker run ...`.
- Kubernetes authorization is namespace-level.
- Wildcards are admin-only by default.
