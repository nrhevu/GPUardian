
## User Guide

Use Rocguard when you need to run workloads on shared GPUs. The usual flow is:
reserve GPU time, copy the returned key, then run or authorize your workload
with that key.

### Sign in

Open the Rocguard gateway URL from your admin and sign in with your Rocguard
username and password.

### Check GPUs

1. Choose a node from the left sidebar.
2. Open the `Schedule` tab.
3. Look at each GPU card:
   - `Available` means it can be reserved.
   - `Reserved` means it already has a scheduled reservation.
   - `Claimed` means it is currently claimed by a running job.
4. Memory and utilization show the current GPU load.

### Reserved vs claimed keys

Rocguard has two key modes:

- `Reserved` keys are for scheduled GPU time. Pick the GPU, start time, and end
  time first, then use the returned key during that window.
- `Claimed` keys are for flexible use. The key is not tied to a schedule. When
  an authorized process starts using a GPU, Rocguard claims that GPU for the
  key. Other users cannot use that GPU until the claim is gone.

Use `Reserved` when you know the exact time and GPUs you need. Use `Claimed`
only when an admin wants a long-lived key for less scheduled workflows.

### Reserve GPU time

1. Select one or more available GPUs.
2. Pick the start and end time.
3. Enter `User` and `Purpose`.
4. Click `Submit`.
5. Copy the returned key.

If the selected time overlaps another reservation, or a selected GPU already has
a running process, Rocguard rejects the reservation and shows a short error.

### Run a command with `rocguard run`

For a normal shell command, run it through the Rocguard wrapper:

```bash
KEY=rg_xxx rocguard run -- python train.py
```

Everything after `--` is your command. Rocguard authorizes that command while it
runs, including child processes. Use the key returned by the reservation. A
reserved key only works during its reserved time window.

More examples:

```bash
KEY=rg_xxx rocguard run -- bash train.sh
KEY=rg_xxx rocguard run -- torchrun --nproc_per_node=8 train.py
```

### Authorize an existing scope with `rocguard allow`

Use `rocguard allow` when the workload is started by another system and cannot
be wrapped directly with `rocguard run`.

Authorize a Docker container:

```bash
KEY=rg_xxx rocguard allow docker --container trainer
```

Authorize a Kubernetes namespace:

```bash
KEY=rg_xxx rocguard allow k8s --namespace training
```

Authorize all processes from one Linux user:

```bash
KEY=rg_xxx rocguard allow user --name alice
```

Wildcard values are supported, for example `training-*` or `codex*`. Keep allow
rules as narrow as possible.

### View or revoke keys

Open the `Key` tab to see keys and reservations without secrets.

- `Show key` asks for the root key before revealing a stored secret.
- `Revoke` removes a key or reservation.

Normal users should not need the root key. Ask an admin if a key must be shown
or revoked and you do not have permission.

### Check status from the CLI

```bash
rocguard status
rocguard ps
KEY=rg_xxx rocguard token info
```

### Simple rules

- Reserve before running shared GPUs.
- Keep reservations short and accurate.
- Revoke reservations you no longer need.
- Do not share your returned key with other users.
- If your job is killed, check whether it was outside its reservation window or
  running on the wrong GPU.
