# GPUardian Python SDK

The Python SDK is a thin client over the GPUardian gateway API. It uses a
scoped access token (`ga_...`) and does not store or submit an account
password.

## Install

```bash
cd sdk/python
python3 -m venv .venv
.venv/bin/pip install -e .
```

## Create a client

Create a token under **Account → Access tokens**, then pass it directly or
load it from your application's secret store:

```python
import os

from gpuardian_sdk import GpuardianClient

client = GpuardianClient(
    "https://gpuardian.example.com:8443",
    os.environ["GPUARDIAN_ACCESS_TOKEN"],
)
client.validate_auth()

nodes = client.list_servers()
reservation = client.create_reservation(
    nodes[0]["id"],
    gpus=[0, 1],
    purpose="training",
    ttl="2h",
)
client.close()
```

Grant only the scopes the application needs. The plaintext token is shown
once, stored only as a hash by the gateway, expires automatically, and can be
revoked without changing the account password.

## Available methods

- `list_servers()` and `fleet_snapshot()`
- `create_reservation()` and `revoke()`
- `allow()`
- `list_keys()`, `reveal_key()`, and `regenerate_key()`
- `history_summary()`, `history_search()`, `history_session()`, and
  `history_session_jobs()`

The gateway enforces the token scopes and the account's role. SDK requests do
not gain broader access than the same account has in the web UI.

## Command-line interface

Installing the package also installs `gpuardian-sdk`. Configure the gateway
and scoped access token through environment variables so the token does not
appear in shell history:

```bash
export GPUARDIAN_URL=https://gpuardian.example.com:8443
export GPUARDIAN_ACCESS_TOKEN=ga_xxx

gpuardian-sdk auth
gpuardian-sdk nodes
gpuardian-sdk fleet
```

Create a reservation on any registered node by exact node name or ID:

```bash
gpuardian-sdk reserve --node node-a --gpus 0,1 --purpose training --ttl 2h
gpuardian-sdk reserve --node node-a --gpus 0,1 \
  --starts-at 2026-09-16T01:00:00Z --expires-at 2026-09-16T03:00:00Z
gpuardian-sdk revoke --node node-a --id grp_xxx
```

Authorize an external workload scope:

```bash
gpuardian-sdk allow docker --node node-a --container trainer --run-name training
gpuardian-sdk allow k8s --node node-a --namespace training --run-name training
gpuardian-sdk allow user --node node-a --user alice --run-name training
```

Key and history commands mirror the remaining SDK methods:

```bash
gpuardian-sdk keys list
gpuardian-sdk keys reveal --user alice
gpuardian-sdk keys rotate --user alice
gpuardian-sdk history summary --node srv_xxx --limit 20
gpuardian-sdk history session --id sess_xxx
gpuardian-sdk history jobs --id sess_xxx --limit 100
```

All successful commands write formatted JSON to stdout. Errors go to stderr
and return a non-zero exit status. Use `--insecure` only for a trusted test
gateway whose certificate cannot be verified.
