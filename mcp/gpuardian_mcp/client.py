"""HTTP client for the Gpuardian web gateway API.

Uses a scoped MCP Bearer token by default. Username/password login remains as
a temporary compatibility path, but the password is discarded after login and
is never reused automatically.
"""

from __future__ import annotations

from typing import Any
from urllib.parse import quote

import httpx


class GpuardianError(Exception):
    """Raised when the gateway returns an error response."""


class GpuardianClient:
    """Thin HTTP client over the Gpuardian web gateway /api/* endpoints."""

    def __init__(
        self,
        base_url: str,
        username: str | None = None,
        password: str | None = None,
        *,
        token: str | None = None,
        timeout: float = 30.0,
        verify_tls: bool = True,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._username = username
        self._password = password
        self._token = token
        if not token and (not username or not password):
            raise ValueError("token or username/password is required")
        headers = {"Accept": "application/json"}
        if token:
            headers["Authorization"] = f"Bearer {token}"
        self._client = httpx.Client(
            base_url=self._base_url,
            cookies=httpx.Cookies(),
            timeout=timeout,
            verify=verify_tls,
            follow_redirects=False,
            headers=headers,
        )
        self._logged_in = bool(token)

    # ------------------------------------------------------------------
    # Auth
    # ------------------------------------------------------------------

    def login(self) -> None:
        """Authenticate once with the compatibility password flow."""
        if self._token:
            return
        if not self._username or self._password is None:
            raise GpuardianError("password session expired; configure an MCP access token")
        password = self._password
        self._password = None
        resp = self._client.post(
            "/api/login",
            json={"username": self._username, "password": password},
        )
        if resp.status_code == 429:
            retry = resp.headers.get("Retry-After")
            raise GpuardianError(
                f"login rate-limited, retry after {retry}s" if retry else "login rate-limited"
            )
        if resp.status_code != 200:
            raise GpuardianError(f"login failed: HTTP {resp.status_code}")
        self._logged_in = True

    def validate_auth(self) -> None:
        """Fail fast when the configured credential is invalid or expired."""
        if not self._token:
            self.login()
            return
        resp = self._client.get("/api/session")
        if resp.status_code != 200:
            raise GpuardianError(f"credential validation failed: HTTP {resp.status_code}")
        body = resp.json()
        if not body.get("authenticated"):
            raise GpuardianError("MCP access token is invalid, expired, or revoked")

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
        if not self._logged_in:
            self.login()
        resp = self._client.request(
            method,
            path,
            params=_drop_none(params) if params else None,
            json=json_body,
        )
        if resp.status_code == 401:
            self._logged_in = False
            if self._token:
                raise GpuardianError("MCP access token is invalid, expired, or revoked")
            raise GpuardianError("password session expired; restart with an MCP access token")
        if resp.status_code == 429:
            retry = resp.headers.get("Retry-After")
            raise GpuardianError(
                f"rate-limited ({path}), retry after {retry}s" if retry else f"rate-limited ({path})"
            )
        if resp.status_code >= 400:
            detail = _error_detail(resp)
            raise GpuardianError(f"HTTP {resp.status_code} {method} {path}: {detail}")
        if resp.status_code == 204 or not resp.content:
            return None
        return resp.json()

    # ------------------------------------------------------------------
    # Servers / fleet
    # ------------------------------------------------------------------

    def list_servers(self) -> list[dict[str, Any]]:
        """GET /api/servers — list registered nodes (no root key)."""
        return self._request("GET", "/api/servers")

    def fleet_snapshot(self) -> dict[str, Any]:
        """GET /api/fleet/snapshot — aggregate snapshot of all nodes."""
        return self._request("GET", "/api/fleet/snapshot")

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

    def list_keys(self) -> list[dict[str, Any]]:
        """GET /api/keys — list fixed user keys (admin: all, user: own)."""
        return self._request("GET", "/api/keys")

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
    ) -> dict[str, Any]:
        """POST /api/history/search — search reservation sessions."""
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
        return self._request("POST", "/api/history/search", json_body=body)

    def history_session(self, session_id: str) -> dict[str, Any]:
        """GET /api/history/sessions/{id} — full session record."""
        return self._request(
            "GET",
            f"/api/history/sessions/{quote(session_id, safe='')}",
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
