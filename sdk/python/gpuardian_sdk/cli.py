"""Command-line interface for the GPUardian gateway SDK."""

from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Any, Sequence

import httpx

from .client import GpuardianClient, GpuardianError


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="gpuardian-sdk",
        description="Manage GPUardian through the web gateway API.",
    )
    parser.add_argument(
        "--url",
        default=os.environ.get("GPUARDIAN_URL", ""),
        help="Gateway URL (or set GPUARDIAN_URL).",
    )
    parser.add_argument("--timeout", type=float, default=30.0, help="Request timeout in seconds.")
    parser.add_argument("--insecure", action="store_true", help="Disable gateway TLS verification.")

    commands = parser.add_subparsers(dest="command", required=True)

    auth = commands.add_parser("auth", help="Validate the configured access token.")
    auth.set_defaults(handler=_auth)

    nodes = commands.add_parser("nodes", help="List registered GPU nodes.")
    nodes.set_defaults(handler=_nodes)

    fleet = commands.add_parser("fleet", help="Get the current fleet snapshot.")
    fleet.set_defaults(handler=_fleet)

    reserve = commands.add_parser("reserve", help="Create a reservation on a node.")
    _add_node_argument(reserve)
    reserve.add_argument("--gpus", type=_gpu_list, help="Comma-separated GPU indexes, for example 0,1.")
    reserve.add_argument("--purpose", help="Reservation purpose or label.")
    reserve.add_argument("--ttl", help="Duration such as 2h; defaults to 1h.")
    reserve.add_argument("--starts-at", help="RFC3339 start time.")
    reserve.add_argument("--expires-at", help="RFC3339 end time.")
    reserve.add_argument("--mode", help="Optional reservation mode supported by the node.")
    reserve.set_defaults(handler=_reserve)

    revoke = commands.add_parser("revoke", help="Revoke a reservation or authorization.")
    _add_node_argument(revoke)
    revoke.add_argument("--id", required=True, help="Reservation, group, or authorization ID.")
    revoke.set_defaults(handler=_revoke)

    allow = commands.add_parser("allow", help="Authorize a workload scope on a node.")
    allow_modes = allow.add_subparsers(dest="allow_mode", required=True)
    allow_specs = (
        ("docker", "--container", "Container name, ID, or supported pattern."),
        ("k8s", "--namespace", "Kubernetes namespace."),
        ("user", "--user", "Linux username."),
    )
    for mode, option, help_text in allow_specs:
        child = allow_modes.add_parser(mode)
        _add_node_argument(child)
        child.add_argument(option, required=True, help=help_text)
        child.add_argument("--run-name", default="Claimed run", help="Activity label.")
        child.set_defaults(handler=_allow)

    keys = commands.add_parser("keys", help="Inspect or rotate fixed user keys.")
    key_commands = keys.add_subparsers(dest="keys_command", required=True)
    key_list = key_commands.add_parser("list")
    key_list.set_defaults(handler=_keys_list)
    key_reveal = key_commands.add_parser("reveal")
    key_reveal.add_argument("--user", required=True)
    key_reveal.set_defaults(handler=_keys_reveal)
    key_rotate = key_commands.add_parser("rotate")
    key_rotate.add_argument("--user", required=True)
    key_rotate.set_defaults(handler=_keys_rotate)

    history = commands.add_parser("history", help="Read reservation and workload history.")
    history_commands = history.add_subparsers(dest="history_command", required=True)
    summary = history_commands.add_parser("summary")
    summary.add_argument("--node", dest="server_id", help="Node ID filter.")
    summary.add_argument("--owner")
    summary.add_argument("--status")
    summary.add_argument("--from", dest="from_")
    summary.add_argument("--to")
    _add_page_arguments(summary)
    summary.set_defaults(handler=_history_summary)
    search = history_commands.add_parser("search")
    search.add_argument("--filters-json", type=_filter_groups, help="JSON array of history filter groups.")
    search.add_argument("--sort-field")
    search.add_argument("--sort-direction", choices=("asc", "desc"))
    _add_page_arguments(search)
    search.set_defaults(handler=_history_search)
    session = history_commands.add_parser("session")
    session.add_argument("--id", required=True, dest="session_id")
    session.set_defaults(handler=_history_session)
    jobs = history_commands.add_parser("jobs")
    jobs.add_argument("--id", required=True, dest="session_id")
    _add_page_arguments(jobs)
    jobs.set_defaults(handler=_history_jobs)

    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    token = os.environ.get("GPUARDIAN_ACCESS_TOKEN", "").strip()
    if not args.url:
        print("error: gateway URL is required; use --url or GPUARDIAN_URL", file=sys.stderr)
        return 2
    if not token:
        print("error: GPUARDIAN_ACCESS_TOKEN is required", file=sys.stderr)
        return 2
    if args.timeout <= 0:
        print("error: --timeout must be greater than zero", file=sys.stderr)
        return 2

    client = None
    try:
        client = GpuardianClient(
            args.url,
            token,
            timeout=args.timeout,
            verify_tls=not args.insecure,
        )
        result = args.handler(client, args)
        _print_json(result)
        return 0
    except (GpuardianError, httpx.HTTPError, ValueError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    finally:
        if client is not None:
            client.close()


def _add_node_argument(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--node", required=True, help="Exact node ID or name.")


def _add_page_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--limit", type=int)
    parser.add_argument("--cursor")


def _gpu_list(value: str) -> list[int]:
    try:
        gpus = [int(item.strip()) for item in value.split(",") if item.strip()]
    except ValueError as error:
        raise argparse.ArgumentTypeError("GPU indexes must be integers") from error
    if not gpus or any(gpu < 0 for gpu in gpus) or len(gpus) != len(set(gpus)):
        raise argparse.ArgumentTypeError("GPU indexes must be unique non-negative integers")
    return gpus


def _filter_groups(value: str) -> list[dict[str, Any]]:
    try:
        groups = json.loads(value)
    except json.JSONDecodeError as error:
        raise argparse.ArgumentTypeError(f"invalid JSON: {error.msg}") from error
    if not isinstance(groups, list) or any(not isinstance(group, dict) for group in groups):
        raise argparse.ArgumentTypeError("filters must be a JSON array of objects")
    return groups


def _resolve_node(client: GpuardianClient, reference: str) -> str:
    servers = client.list_servers()
    for server in servers:
        if server.get("id") == reference:
            return reference
    matches = [server for server in servers if server.get("name") == reference]
    if len(matches) == 1 and matches[0].get("id"):
        return str(matches[0]["id"])
    if len(matches) > 1:
        raise ValueError(f"node name is ambiguous: {reference}")
    raise ValueError(f"node not found: {reference}")


def _auth(client: GpuardianClient, _args: argparse.Namespace) -> dict[str, bool]:
    client.validate_auth()
    return {"authenticated": True}


def _nodes(client: GpuardianClient, _args: argparse.Namespace) -> Any:
    return client.list_servers()


def _fleet(client: GpuardianClient, _args: argparse.Namespace) -> Any:
    return client.fleet_snapshot()


def _reserve(client: GpuardianClient, args: argparse.Namespace) -> Any:
    return client.create_reservation(
        _resolve_node(client, args.node),
        gpus=args.gpus,
        purpose=args.purpose,
        ttl=args.ttl,
        starts_at=args.starts_at,
        expires_at=args.expires_at,
        mode=args.mode,
    )


def _revoke(client: GpuardianClient, args: argparse.Namespace) -> Any:
    return client.revoke(_resolve_node(client, args.node), args.id)


def _allow(client: GpuardianClient, args: argparse.Namespace) -> Any:
    return client.allow(
        _resolve_node(client, args.node),
        mode=args.allow_mode,
        run_name=args.run_name,
        container=getattr(args, "container", None),
        namespace=getattr(args, "namespace", None),
        user=getattr(args, "user", None),
    )


def _keys_list(client: GpuardianClient, _args: argparse.Namespace) -> Any:
    return client.list_keys()


def _keys_reveal(client: GpuardianClient, args: argparse.Namespace) -> Any:
    return client.reveal_key(args.user)


def _keys_rotate(client: GpuardianClient, args: argparse.Namespace) -> Any:
    return client.regenerate_key(args.user)


def _history_summary(client: GpuardianClient, args: argparse.Namespace) -> Any:
    return client.history_summary(
        server_id=args.server_id,
        owner=args.owner,
        status=args.status,
        from_=args.from_,
        to=args.to,
        limit=args.limit,
        cursor=args.cursor,
    )


def _history_search(client: GpuardianClient, args: argparse.Namespace) -> Any:
    return client.history_search(
        filter_groups=args.filters_json,
        sort_field=args.sort_field,
        sort_direction=args.sort_direction,
        limit=args.limit,
        cursor=args.cursor,
    )


def _history_session(client: GpuardianClient, args: argparse.Namespace) -> Any:
    return client.history_session(args.session_id)


def _history_jobs(client: GpuardianClient, args: argparse.Namespace) -> Any:
    return client.history_session_jobs(args.session_id, limit=args.limit, cursor=args.cursor)


def _print_json(value: Any) -> None:
    json.dump(value, sys.stdout, ensure_ascii=False, indent=2, sort_keys=True)
    sys.stdout.write("\n")


if __name__ == "__main__":
    raise SystemExit(main())
