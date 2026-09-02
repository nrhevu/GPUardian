"""Entry point for the Gpuardian MCP server (stdio transport).

Configuration via environment variables:

  GPUARDIAN_MCP_URL       Web gateway URL (required)
  GPUARDIAN_MCP_TOKEN     Scoped MCP access token (recommended)
  GPUARDIAN_MCP_USER      Username (legacy fallback)
  GPUARDIAN_MCP_PASSWORD  Password (legacy fallback)
  GPUARDIAN_MCP_TIMEOUT   HTTP timeout in seconds (default 30)
  GPUARDIAN_MCP_VERIFY_TLS  Verify TLS certificates, 1/0 (default 1)
"""

from __future__ import annotations

import os
import sys

from mcp.server.stdio import stdio_server

from .client import GpuardianClient, GpuardianError
from .server import create_server


def _env(name: str, default: str | None = None) -> str:
    value = os.environ.get(name)
    if value is None or value == "":
        if default is None:
            print(f"gpuardian-mcp: missing required env var {name}", file=sys.stderr)
            sys.exit(1)
        return default
    return value


def main() -> None:
    base_url = _env("GPUARDIAN_MCP_URL")
    token = os.environ.get("GPUARDIAN_MCP_TOKEN", "").strip()
    username = None
    password = None
    if not token:
        username = _env("GPUARDIAN_MCP_USER")
        password = _env("GPUARDIAN_MCP_PASSWORD")
        print(
            "gpuardian-mcp: password login is deprecated; create a scoped MCP access token",
            file=sys.stderr,
        )
    timeout = float(_env("GPUARDIAN_MCP_TIMEOUT", "30"))
    verify_tls = _env("GPUARDIAN_MCP_VERIFY_TLS", "1") != "0"

    client = GpuardianClient(
        base_url=base_url,
        username=username,
        password=password,
        token=token or None,
        timeout=timeout,
        verify_tls=verify_tls,
    )

    # Validate eagerly so config errors surface before the MCP loop starts.
    try:
        client.validate_auth()
    except GpuardianError as exc:
        print(f"gpuardian-mcp: authentication failed: {exc}", file=sys.stderr)
        sys.exit(1)

    app = create_server(client)

    import asyncio

    async def run() -> None:
        async with stdio_server() as (read_stream, write_stream):
            await app.run(read_stream, write_stream, app.create_initialization_options())

    try:
        asyncio.run(run())
    finally:
        client.close()


if __name__ == "__main__":
    main()
