---
name: mcp-dev
description: How to set up and run the GPUardian MCP server — the Python stdio server that exposes reservation operations as MCP tools for AI assistants like Claude or Cursor. Use whenever the user mentions MCP, connecting an AI assistant to GPUardian, MCP tools, gpuardian-mcp, or wants an LLM to reserve/manage GPUs — even if they just say "let Claude use GPUardian" or "MCP integration".
---

# Set up and run the GPUardian MCP server

The MCP server (`mcp/gpuardian_mcp/`) is a Python 3.11+ stdio server. It is
**not** part of the Go build — it has its own venv and is a thin HTTP client
over the web gateway's `/api/*` endpoints. It authenticates with a GPUardian
**username + password** (not a `gk_` key, not an `rk_` root key). All
authorization is enforced server-side by the gateway.

## 1. Install (one-time)

```bash
cd mcp
python3 -m venv .venv
.venv/bin/pip install -e .
```

Deps: `mcp>=1.0.0`, `httpx>=0.27.0` (see `mcp/pyproject.toml`). The console
script `gpuardian-mcp` is installed; you can also run
`.venv/bin/python -m gpuardian_mcp` directly.

## 2. Configure via env vars (no CLI flags)

All config is `GPUARDIAN_MCP_*`. The server is stdio-only.

| Env var | Required | Default | Purpose |
|---|---|---|---|
| `GPUARDIAN_MCP_URL` | yes | — | Gateway URL (`http://127.0.0.1:18080` dev, `https://gpuardian.example.com:8443` prod) |
| `GPUARDIAN_MCP_USER` | yes | — | GPUardian username |
| `GPUARDIAN_MCP_PASSWORD` | yes | — | GPUardian password |
| `GPUARDIAN_MCP_TIMEOUT` | no | `30` | HTTP timeout (seconds) |
| `GPUARDIAN_MCP_VERIFY_TLS` | no | `1` | Verify TLS; set `0` **only** for dev self-signed certs |

Missing required vars → the server prints
`gpuardian-mcp: missing required env var <NAME>` to stderr and exits 1.

## 3. Run it

```bash
GPUARDIAN_MCP_URL=http://127.0.0.1:18080 \
GPUARDIAN_MCP_USER=<username> \
GPUARDIAN_MCP_PASSWORD=<password> \
.venv/bin/python -m gpuardian_mcp
```

The server **logs in eagerly at startup** so bad credentials fail fast before
the MCP loop begins. If the session expires mid-conversation, the client
auto-re-logs-in on a 401 and retries — no manual re-auth needed.

## 4. Wire it into an MCP client (AI assistant)

Add a `mcpServers` block to the client's config:

```json
{
  "mcpServers": {
    "gpuardian": {
      "command": "/path/to/gpuardian/mcp/.venv/bin/python",
      "args": ["-m", "gpuardian_mcp"],
      "env": {
        "GPUARDIAN_MCP_URL": "http://127.0.0.1:18080",
        "GPUARDIAN_MCP_USER": "<username>",
        "GPUARDIAN_MCP_PASSWORD": "<password>"
      },
      "cwd": "/path/to/gpuardian/mcp"
    }
  }
}
```

## 5. Test interactively with the MCP Inspector

```bash
.venv/bin/mcp dev gpuardian_mcp.__main__
```

## 6. The 12 tools exposed

| Tool | What it does |
|---|---|
| `list_servers` | List registered GPU nodes |
| `fleet_snapshot` | Live snapshot of all nodes (GPUs, reservations, tokens, authorizations, processes) |
| `create_reservation` | Reserve GPUs on a node (`server_id`, `gpus`, `purpose`, `ttl` or `starts_at`+`expires_at`, `mode`) |
| `revoke` | Revoke a reservation/token/auth by ID |
| `list_keys` | List fixed user keys with node sync status |
| `reveal_key` | Reveal the plaintext `gk_...` secret for a user |
| `regenerate_key` | Rotate a user's fixed key |
| `allow` | Grant an authorization scope (`docker`/`k8s`/`user`) on a node; requires `run_name` for the claimed activity label |
| `history_summary` | Dashboard summary with optional filters |
| `history_search` | Search sessions with filters + sorting + pagination |
| `history_session` | Full record of one session by ID |
| `history_session_jobs` | Paginated jobs for a session |

Errors come back as `TextContent` (`Gpuardian error: ...`), never raised to
the MCP client.

## Authorization model

- A **regular user** sees only their own resources: `fleet_snapshot`,
  `list_keys`, `history_*` are filtered to their own; `reveal_key`,
  `regenerate_key`, `revoke` work only on their own; `allow` wildcards are
  forbidden.
- An **admin** sees and can act on everything.
- The MCP user **never sees the raw node token** — the gateway strips
  `Token`/`TokenID` from reservation responses. To run the node CLI, retrieve
  the `gk_...` key via `reveal_key` and use `KEY=gk_... gpuardian run ...`.

## What the user needs from the operator

1. A GPUardian **account** (username + password) — either self-registered if
   `GPUARDIAN_WEB_ALLOW_REGISTRATION=1`, or admin-created.
2. The **gateway URL**.
3. (Optional, for the node CLI only) their `gk_...` fixed key, obtainable via
   the `reveal_key` tool. The MCP server itself does **not** need it.

They do **not** need a root key (`rk_...`), node access, or Docker access —
those are operator-only.

## Gotchas

- **The MCP server is env-var driven and stdio-only.** Do not add CLI flags
  or duplicate reservation logic in Python — keep it a thin client over the
  gateway HTTP API.
- **No test suite.** Verify by hand against a running gateway.
- **Dev vs prod:** dev gateway is loopback `18080` HTTP (`VERIFY_TLS=0` ok);
  prod is `8443` HTTPS (`VERIFY_TLS=1`, the default).

## Read before sensitive edits

- `mcp/README.md` — the canonical MCP setup doc, tool table, security notes.
- `AGENTS.md` — "The MCP server is env-var driven and stdio-only" convention.
