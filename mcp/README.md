# GPUardian MCP Server

An [MCP](https://modelcontextprotocol.io/) server that exposes GPUardian GPU
reservation operations as tools for AI assistants. The server connects to the
GPUardian web gateway over HTTP, authenticates with a username and password, and
speaks the MCP protocol over stdio.

## Requirements

- Python 3.11 or newer
- A running GPUardian web gateway (production or dev)
- Credentials for a GPUardian account (admin or regular user)

## Installation

```bash
cd mcp
python3 -m venv .venv
.venv/bin/pip install -e .
```

Or with [uv](https://docs.astral.sh/uv/):

```bash
cd mcp
uv pip install -e .
```

## Configuration

All configuration is via environment variables:

| Env var | Required | Default | Description |
| --- | --- | --- | --- |
| `GPUARDIAN_MCP_URL` | yes | — | Web gateway URL, e.g. `http://127.0.0.1:18080` (dev) or `https://gpuardian.example.com:8443` (prod) |
| `GPUARDIAN_MCP_TOKEN` | recommended | — | Scoped `ga_...` MCP access token created from the account menu |
| `GPUARDIAN_MCP_USER` | legacy | — | GPUardian username; only used when no token is configured |
| `GPUARDIAN_MCP_PASSWORD` | legacy | — | GPUardian password; only used when no token is configured |
| `GPUARDIAN_MCP_TIMEOUT` | no | `30` | HTTP timeout in seconds |
| `GPUARDIAN_MCP_VERIFY_TLS` | no | `1` | Verify TLS certificates (`1`/`0`). Set to `0` only for dev with self-signed certs. |

Create a token from **Account → MCP access tokens** in the web UI. Give each
MCP client its own name, expiry, and minimum required scopes. The plaintext
secret is shown once; the gateway stores only its SHA-256 hash. A revoked or
expired token fails closed and is never refreshed automatically.

Username/password login remains temporarily available for compatibility. The
password is discarded from memory after the initial login and is not reused
when the 24-hour browser session expires. Restarting the MCP server is then
required. New installations should use a token.

## Running

```bash
GPUARDIAN_MCP_URL=http://127.0.0.1:18080 \
GPUARDIAN_MCP_TOKEN=ga_your_scoped_token \
.venv/bin/python -m gpuardian_mcp
```

Or via the installed entry point:

```bash
GPUARDIAN_MCP_URL=... GPUARDIAN_MCP_TOKEN=ga_your_scoped_token \
gpuardian-mcp
```

## MCP client configuration

### MCP-compatible client

Add the following to your client's MCP server config. The block below uses the
standard `mcpServers` shape that most MCP clients accept; consult your
client's docs for the exact file path and any client-specific fields.

```json
{
  "mcpServers": {
    "gpuardian": {
      "command": "/path/to/gpuardian/mcp/.venv/bin/python",
      "args": ["-m", "gpuardian_mcp"],
      "env": {
        "GPUARDIAN_MCP_URL": "http://127.0.0.1:18080",
        "GPUARDIAN_MCP_TOKEN": "ga_your_scoped_token"
      },
      "cwd": "/path/to/gpuardian/mcp"
    }
  }
}
```

### MCP Inspector (for testing)

```bash
.venv/bin/mcp dev gpuardian_mcp.__main__
```

## Tools

| Tool | Description |
| --- | --- |
| `list_servers` | List registered GPU nodes. |
| `fleet_snapshot` | Live snapshot of all nodes: GPUs, reservations, tokens, authorizations. |
| `create_reservation` | Reserve GPUs on a node (GPUs, purpose, TTL or time window). |
| `revoke` | Revoke a reservation/token/authorization by ID. |
| `list_keys` | List fixed user keys (admin: all, user: own). |
| `reveal_key` | Reveal the plaintext key secret (`gk_...`) for a user. |
| `regenerate_key` | Rotate a user's fixed key. |
| `history_summary` | Dashboard summary of reservation history with optional filters. |
| `history_search` | Search reservation sessions with filter groups and sorting. |
| `history_session` | Full record of a single reservation session. |
| `history_session_jobs` | Paginated list of observed jobs for a session. |
| `allow` | Grant an authorization scope (docker/k8s/user) on a node; `run_name` is required and labels claimed activity in GPU Activity. |

Example `allow` arguments:

```json
{
  "server_id": "node-id",
  "mode": "docker",
  "container": "trainer",
  "run_name": "GLM TP4 benchmark"
}
```

## Security notes

- Use one short-lived token per MCP client and grant only the scopes it needs.
- The default scopes exclude `keys:reveal`, `keys:rotate`, and
  `history:write`. Add these only when the assistant genuinely needs them.
- MCP access tokens can be revoked independently without changing the account
  password or ending browser sessions.
- The `reveal_key` tool returns the key secret in cleartext. Only use it when
  the token explicitly has `keys:reveal` and the assistant's output is visible
  to an authorized user.
- Non-admin accounts see only their own resources (the gateway enforces this).
- Tokens, passwords, and cookies are never written to stdout — stdout carries
  only MCP protocol messages. Errors go to stderr.
- For production, always use HTTPS (`GPUARDIAN_MCP_VERIFY_TLS=1`).
