"""Token-authenticated client for the GPUardian web gateway API."""

from __future__ import annotations

from typing import Any
from urllib.parse import quote

import httpx


class GpuardianError(Exception):
    """Raised when the gateway returns an error response.

    ``status_code`` and ``detail`` carry the HTTP status and the gateway's
    ``error`` message when the failure came from an HTTP response.
    """

    def __init__(
        self,
        message: str,
        *,
        status_code: int | None = None,
        detail: str | None = None,
    ) -> None:
        super().__init__(message)
        self.status_code = status_code
        self.detail = detail


class GpuardianClient:
    """Thin HTTP client over the Gpuardian web gateway /api/* endpoints."""

    def __init__(
        self,
        base_url: str,
        access_token: str,
        *,
        timeout: float = 30.0,
        verify_tls: bool = True,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        access_token = access_token.strip()
        if not access_token:
            raise ValueError("access_token is required")
        self._client = httpx.Client(
            base_url=self._base_url,
            timeout=timeout,
            verify=verify_tls,
            follow_redirects=False,
            headers={
                "Accept": "application/json",
                "Authorization": f"Bearer {access_token}",
            },
        )

    # ------------------------------------------------------------------
    # Auth
    # ------------------------------------------------------------------

    def validate_auth(self) -> None:
        """Fail fast when the configured credential is invalid or expired."""
        resp = self._client.get("/api/session")
        if resp.status_code != 200:
            raise GpuardianError(f"credential validation failed: HTTP {resp.status_code}")
        body = resp.json()
        if not body.get("authenticated"):
            raise GpuardianError("access token is invalid, expired, or revoked")

    def close(self) -> None:
        self._client.close()

    # ------------------------------------------------------------------
    # Internal request helper
    # ------------------------------------------------------------------

    def _request(
        self,
        method: str,
        path: str,
        *,
        params: dict[str, Any] | None = None,
        json_body: dict[str, Any] | None = None,
    ) -> Any:
        resp = self._client.request(
            method,
            path,
            params=_drop_none(params) if params else None,
            json=json_body,
        )
        if resp.status_code == 401:
            raise GpuardianError(
                "access token is invalid, expired, or revoked", status_code=401
            )
        if resp.status_code == 429:
            retry = resp.headers.get("Retry-After")
            raise GpuardianError(
                f"rate-limited ({path}), retry after {retry}s" if retry else f"rate-limited ({path})",
                status_code=429,
            )
        if resp.status_code >= 400:
            detail = _error_detail(resp)
            raise GpuardianError(
                f"HTTP {resp.status_code} {method} {path}: {detail}",
                status_code=resp.status_code,
                detail=detail,
            )
        if resp.status_code == 204 or not resp.content:
            return None
        return resp.json()

    # ------------------------------------------------------------------
    # Servers / fleet
    # ------------------------------------------------------------------

    def list_servers(self) -> list[dict[str, Any]]:
        """GET /api/servers — list registered nodes (no root key)."""
        return self._request("GET", "/api/servers")

    def fleet_snapshot(self, *, compact: bool = False) -> dict[str, Any]:
        """GET /api/fleet/snapshot — aggregate snapshot of all nodes.

        ``compact=True`` asks the gateway to omit per-node tokens, soft claims,
        leases, bypasses, and ps rows (GPUs with their processes, reservations,
        and authorizations are kept).
        """
        return self._request(
            "GET",
            "/api/fleet/snapshot",
            params={"compact": "1"} if compact else None,
        )

    # ------------------------------------------------------------------
    # Reservations
    # ------------------------------------------------------------------

    def create_reservation(
        self,
        server_id: str,
        *,
        gpus: list[int] | None = None,
        purpose: str | None = None,
        ttl: str | None = None,
        starts_at: str | None = None,
        expires_at: str | None = None,
        mode: str | None = None,
    ) -> dict[str, Any]:
        """POST /api/servers/{id}/reservations — create a GPU reservation."""
        body: dict[str, Any] = {"ttl": ttl or "1h"}
        if gpus is not None:
            body["gpus"] = gpus
        if purpose is not None:
            body["purpose"] = purpose
        if starts_at is not None:
            body["starts_at"] = starts_at
        if expires_at is not None:
            body["expires_at"] = expires_at
        if mode is not None:
            body["mode"] = mode
        return self._request(
            "POST",
            f"/api/servers/{quote(server_id, safe='')}/reservations",
            json_body=body,
        )

    def revoke(self, server_id: str, id: str) -> dict[str, Any]:
        """POST /api/servers/{id}/revoke — revoke a reservation/token/auth."""
        return self._request(
            "POST",
            f"/api/servers/{quote(server_id, safe='')}/revoke",
            json_body={"id": id},
        )

    # ------------------------------------------------------------------
    # Keys
    # ------------------------------------------------------------------

    def list_keys(self, *, include_node_sync: bool = True) -> list[dict[str, Any]]:
        """GET /api/keys — list fixed user keys (admin: all, user: own).

        Per-key ``node_sync`` status is requested explicitly so the result keeps
        it on gateways that omit it by default; pass ``include_node_sync=False``
        to skip it.
        """
        return self._request(
            "GET",
            "/api/keys",
            params={"include": "node_sync"} if include_node_sync else None,
        )

    def reveal_key(self, username: str) -> dict[str, Any]:
        """POST /api/keys/{username}/reveal — decrypt and return the key secret."""
        return self._request(
            "POST",
            f"/api/keys/{quote(username, safe='')}/reveal",
        )

    def regenerate_key(self, username: str) -> dict[str, Any]:
        """POST /api/keys/{username}/regenerate — rotate the fixed key."""
        return self._request(
            "POST",
            f"/api/keys/{quote(username, safe='')}/regenerate",
        )

    # ------------------------------------------------------------------
    # History & telemetry
    # ------------------------------------------------------------------

    def history_summary(
        self,
        *,
        server_id: str | None = None,
        owner: str | None = None,
        status: str | None = None,
        from_: str | None = None,
        to: str | None = None,
        limit: int | None = None,
        cursor: str | None = None,
    ) -> dict[str, Any]:
        """GET /api/history/summary — dashboard summary."""
        return self._request(
            "GET",
            "/api/history/summary",
            params={
                "server_id": server_id,
                "owner": owner,
                "status": status,
                "from": from_,
                "to": to,
                "limit": limit,
                "cursor": cursor,
            },
        )

    def history_search(
        self,
        *,
        filter_groups: list[dict[str, Any]] | None = None,
        sort_field: str | None = None,
        sort_direction: str | None = None,
        limit: int | None = None,
        cursor: str | None = None,
        include_summary: bool | None = None,
    ) -> dict[str, Any]:
        """POST /api/history/search — search reservation sessions.

        The gateway includes the dashboard ``summary`` unless
        ``include_summary=False``. The summary depends only on the filter, so
        callers fetching further ``cursor`` pages can skip recomputing it.
        """
        body: dict[str, Any] = {}
        if filter_groups is not None:
            body["filter"] = {"groups": filter_groups}
        if sort_field is not None or sort_direction is not None:
            body["sort"] = {
                "field": sort_field or "starts_at",
                "direction": sort_direction or "desc",
            }
        if limit is not None:
            body["limit"] = limit
        if cursor is not None:
            body["cursor"] = cursor
        if include_summary is not None:
            body["include_summary"] = include_summary
        return self._request("POST", "/api/history/search", json_body=body)

    def history_session(
        self,
        session_id: str,
        *,
        timeline_limit: int | None = None,
    ) -> dict[str, Any]:
        """GET /api/history/sessions/{id} — full session record.

        ``timeline_limit`` keeps only the most recent N timeline rows.
        """
        return self._request(
            "GET",
            f"/api/history/sessions/{quote(session_id, safe='')}",
            params={"timeline_limit": timeline_limit},
        )

    def history_session_jobs(
        self,
        session_id: str,
        *,
        limit: int | None = None,
        cursor: str | None = None,
    ) -> dict[str, Any]:
        """GET /api/history/sessions/{id}/jobs — paginated jobs."""
        return self._request(
            "GET",
            f"/api/history/sessions/{quote(session_id, safe='')}/jobs",
            params={"limit": limit, "cursor": cursor},
        )

    # ------------------------------------------------------------------
    # Authorization
    # ------------------------------------------------------------------

    def allow(
        self,
        server_id: str,
        *,
        mode: str,
        run_name: str,
        container: str | None = None,
        namespace: str | None = None,
        user: str | None = None,
    ) -> dict[str, Any]:
        """POST /api/servers/{id}/allow — grant an authorization scope."""
        body: dict[str, Any] = {"mode": mode, "run_name": run_name}
        if container is not None:
            body["container"] = container
        if namespace is not None:
            body["namespace"] = namespace
        if user is not None:
            body["user"] = user
        return self._request(
            "POST",
            f"/api/servers/{quote(server_id, safe='')}/allow",
            json_body=body,
        )


# ----------------------------------------------------------------------
# Helpers
# ----------------------------------------------------------------------


def _drop_none(params: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in params.items() if v is not None}


def _error_detail(resp: httpx.Response) -> str:
    try:
        body = resp.json()
        if isinstance(body, dict) and "error" in body:
            return str(body["error"])
        return str(body)
    except Exception:
        return resp.text[:200] if resp.text else ""
