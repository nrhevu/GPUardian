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
