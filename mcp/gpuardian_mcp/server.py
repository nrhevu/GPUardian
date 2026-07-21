"""MCP server exposing Gpuardian GPU reservation operations as tools."""

from __future__ import annotations

import json
from typing import Any

from mcp.server import Server
from mcp.types import TextContent, Tool

from .client import GpuardianClient, GpuardianError

# ----------------------------------------------------------------------
# Tool definitions
# ----------------------------------------------------------------------

TOOLS: list[Tool] = [
    # --- Servers / fleet ---
    Tool(
        name="list_servers",
        description="List all registered Gpuardian GPU nodes.",
        inputSchema={"type": "object", "properties": {}, "required": []},
    ),
    Tool(
        name="fleet_snapshot",
        description=(
            "Get a live snapshot of the entire fleet: GPUs, reservations, "
            "tokens, authorizations, and processes for every node. "
            "Non-admin users see a filtered view (own resources only)."
        ),
        inputSchema={"type": "object", "properties": {}, "required": []},
    ),
    # --- Reservations ---
    Tool(
        name="create_reservation",
        description=(
            "Create a GPU reservation on a node. Specify GPUs, purpose, and "
            "a time window (starts_at/expires_at as RFC3339) or a TTL duration "
            "like '1h', '2h30m'. The reservation uses the caller's fixed key."
        ),
        inputSchema={
            "type": "object",
            "properties": {
                "server_id": {
                    "type": "string",
                    "description": "Node ID from list_servers.",
                },
                "gpus": {
                    "type": "array",
                    "items": {"type": "integer"},
                    "description": "GPU IDs to reserve.",
                },
                "purpose": {
                    "type": "string",
                    "description": "Human-readable purpose for the reservation.",
                },
                "ttl": {
                    "type": "string",
                    "description": "Duration from now, e.g. '1h', '2h30m'. Defaults to '1h'.",
                },
                "starts_at": {
                    "type": "string",
                    "description": "RFC3339 start time. If omitted, starts now.",
                },
                "expires_at": {
                    "type": "string",
                    "description": "RFC3339 end time. Alternative to ttl.",
                },
                "mode": {
                    "type": "string",
                    "enum": ["reserved", "claimed"],
                    "description": "Reservation mode. Defaults to 'reserved'.",
                },
            },
            "required": ["server_id"],
        },
    ),
    Tool(
        name="revoke",
        description=(
            "Revoke a reservation, token, authorization, or group by ID on a "
            "node. Admins can revoke anything; non-admins only their own."
        ),
        inputSchema={
            "type": "object",
            "properties": {
                "server_id": {"type": "string", "description": "Node ID."},
                "id": {"type": "string", "description": "ID of the reservation/token/auth to revoke."},
            },
            "required": ["server_id", "id"],
        },
    ),
    # --- Keys ---
    Tool(
        name="list_keys",
        description=(
            "List fixed user keys. Admins see all users' keys; regular users "
            "see only their own. Includes node sync status."
        ),
        inputSchema={"type": "object", "properties": {}, "required": []},
    ),
    Tool(
        name="reveal_key",
        description=(
            "Reveal the plaintext fixed key secret (gk_...) for a user. "
            "Non-admins can only reveal their own key. Handle with care — "
            "the secret is returned in cleartext."
        ),
        inputSchema={
            "type": "object",
            "properties": {
                "username": {"type": "string", "description": "Username whose key to reveal."},
            },
            "required": ["username"],
        },
    ),
    Tool(
        name="regenerate_key",
        description=(
            "Regenerate (rotate) a user's fixed key. The previous key stops "
            "working after the snapshot reaches each node. Non-admins can "
            "only regenerate their own."
        ),
        inputSchema={
            "type": "object",
            "properties": {
                "username": {"type": "string", "description": "Username whose key to regenerate."},
            },
            "required": ["username"],
        },
    ),
    # --- History & telemetry ---
    Tool(
        name="history_summary",
        description=(
            "Get a dashboard summary of reservation history: session count, "
            "reserved/busy GPU hours, busy ratio, average utilization, "
            "telemetry coverage. Optional filters by server, owner, status, "
            "and time range."
        ),
        inputSchema={
            "type": "object",
            "properties": {
                "server_id": {"type": "string"},
                "owner": {"type": "string", "description": "Filter by reservation owner."},
                "status": {
                    "type": "string",
                    "enum": ["scheduled", "active", "completed", "revoked"],
                },
                "from": {"type": "string", "description": "RFC3339 start of time range."},
                "to": {"type": "string", "description": "RFC3339 end of time range."},
                "limit": {"type": "integer", "minimum": 1, "maximum": 100, "default": 50},
                "cursor": {"type": "string", "description": "Pagination cursor from a previous call."},
            },
            "required": [],
        },
    ),
    Tool(
        name="history_search",
        description=(
            "Search reservation sessions with filter groups and sorting. "
            "Each group is AND-combined; rules inside a group are OR-combined. "
            "Returns sessions, a summary, and a next_cursor for pagination."
        ),
        inputSchema={
            "type": "object",
            "properties": {
                "filter_groups": {
                    "type": "array",
                    "description": "AND-combined filter groups.",
                    "items": {
                        "type": "object",
                        "properties": {
                            "rules": {
                                "type": "array",
                                "items": {
                                    "type": "object",
                                    "properties": {
                                        "field": {
                                            "type": "string",
                                            "description": "e.g. purpose, owner, status, starts_at, gpu, average_utilization_percent, job_count",
                                        },
                                        "operator": {
                                            "type": "string",
                                            "description": "e.g. eq, ne, contains, gt, lt, gte, lte, in",
                                        },
                                        "value": {},
                                    },
                                    "required": ["field", "operator", "value"],
                                },
                            },
                        },
                        "required": ["rules"],
                    },
                },
                "sort_field": {
                    "type": "string",
                    "description": "Field to sort by, e.g. starts_at, average_utilization_percent, job_count.",
                    "default": "starts_at",
                },
                "sort_direction": {"type": "string", "enum": ["asc", "desc"], "default": "desc"},
                "limit": {"type": "integer", "minimum": 1, "maximum": 100, "default": 50},
                "cursor": {"type": "string"},
            },
            "required": [],
        },
    ),
    Tool(
        name="history_session",
        description="Get the full record of a single reservation session by ID.",
        inputSchema={
            "type": "object",
            "properties": {
                "session_id": {"type": "string", "description": "Session ID (sess_...)."},
            },
            "required": ["session_id"],
        },
    ),
    Tool(
        name="history_session_jobs",
        description="List observed jobs for a reservation session, paginated.",
        inputSchema={
            "type": "object",
            "properties": {
                "session_id": {"type": "string"},
                "limit": {"type": "integer", "minimum": 1, "maximum": 100, "default": 50},
                "cursor": {"type": "string"},
            },
            "required": ["session_id"],
        },
    ),
    # --- Authorization ---
    Tool(
        name="allow",
        description=(
            "Grant an authorization scope on a node for the caller's fixed "
            "key. Use mode 'docker' with container, 'k8s' with namespace, "
            "or 'user' with a username. Wildcards are admin-only."
        ),
        inputSchema={
            "type": "object",
            "properties": {
                "server_id": {"type": "string"},
                "mode": {"type": "string", "enum": ["docker", "k8s", "user"]},
                "container": {"type": "string", "description": "Container name or ID (mode=docker)."},
                "namespace": {"type": "string", "description": "K8s namespace (mode=k8s)."},
                "user": {"type": "string", "description": "Username (mode=user)."},
            },
            "required": ["server_id", "mode"],
        },
    ),
]


# ----------------------------------------------------------------------
# Tool dispatch
# ----------------------------------------------------------------------


def _json_text(data: Any) -> list[TextContent]:
    """Serialize data as pretty-printed JSON text content."""
    return [TextContent(type="text", text=json.dumps(data, indent=2, default=str))]


def _dispatch(client: GpuardianClient, name: str, args: dict[str, Any]) -> list[TextContent]:
    if name == "list_servers":
        return _json_text(client.list_servers())
    if name == "fleet_snapshot":
        return _json_text(client.fleet_snapshot())
    if name == "create_reservation":
        return _json_text(
            client.create_reservation(
                args["server_id"],
                gpus=args.get("gpus"),
                purpose=args.get("purpose"),
                ttl=args.get("ttl"),
                starts_at=args.get("starts_at"),
                expires_at=args.get("expires_at"),
                mode=args.get("mode"),
            )
        )
    if name == "revoke":
        return _json_text(client.revoke(args["server_id"], args["id"]))
    if name == "list_keys":
        return _json_text(client.list_keys())
    if name == "reveal_key":
        return _json_text(client.reveal_key(args["username"]))
    if name == "regenerate_key":
        return _json_text(client.regenerate_key(args["username"]))
    if name == "history_summary":
        return _json_text(
            client.history_summary(
                server_id=args.get("server_id"),
                owner=args.get("owner"),
                status=args.get("status"),
                from_=args.get("from"),
                to=args.get("to"),
                limit=args.get("limit"),
                cursor=args.get("cursor"),
            )
        )
    if name == "history_search":
        return _json_text(
            client.history_search(
                filter_groups=args.get("filter_groups"),
                sort_field=args.get("sort_field"),
                sort_direction=args.get("sort_direction"),
                limit=args.get("limit"),
                cursor=args.get("cursor"),
            )
        )
    if name == "history_session":
        return _json_text(client.history_session(args["session_id"]))
    if name == "history_session_jobs":
        return _json_text(
            client.history_session_jobs(
                args["session_id"],
                limit=args.get("limit"),
                cursor=args.get("cursor"),
            )
        )
    if name == "allow":
        return _json_text(
            client.allow(
                args["server_id"],
                mode=args["mode"],
                container=args.get("container"),
                namespace=args.get("namespace"),
                user=args.get("user"),
            )
        )
    raise ValueError(f"Unknown tool: {name}")


# ----------------------------------------------------------------------
# Server factory
# ----------------------------------------------------------------------


def create_server(client: GpuardianClient) -> Server:
    """Create an MCP Server wired to the given GpuardianClient."""
    app = Server("gpuardian")

    @app.list_tools()
    async def list_tools() -> list[Tool]:
        return TOOLS

    @app.call_tool()
    async def call_tool(name: str, arguments: dict[str, Any] | None) -> list[TextContent]:
        if arguments is None:
            arguments = {}
        try:
            return _dispatch(client, name, arguments)
        except GpuardianError as exc:
            return [TextContent(type="text", text=f"Gpuardian error: {exc}")]
        except Exception as exc:
            return [TextContent(type="text", text=f"Error: {type(exc).__name__}: {exc}")]

    return app
