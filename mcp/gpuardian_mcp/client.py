"""HTTP client for the Gpuardian web gateway API.

Wraps the gateway's /api/* endpoints with session-cookie auth and automatic
re-login on 401. Never logs credentials or cookies.
"""

from __future__ import annotations

import os
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
        username: str,
        password: str,
        *,
        timeout: float = 30.0,
        verify_tls: bool = True,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._username = username
        self._password = password
        self._client = httpx.Client(
            base_url=self._base_url,
            cookies=httpx.Cookies(),
            timeout=timeout,
            verify=verify_tls,
            follow_redirects=False,
            headers={"Accept": "application/json"},
        )
        self._logged_in = False

    # ------------------------------------------------------------------
    # Auth
    # ------------------------------------------------------------------

    def login(self) -> None:
        """Authenticate with the gateway and store the session cookie."""
        resp = self._client.post(
            "/api/login",
            json={"username": self._username, "password": self._password},
        )
        if resp.status_code == 429:
            retry = resp.headers.get("Retry-After")
            raise GpuardianError(
                f"login rate-limited, retry after {retry}s" if retry else "login rate-limited"
            )
        if resp.status_code != 200:
            raise GpuardianError(f"login failed: HTTP {resp.status_code}")
        self._logged_in = True

    def close(self) -> None:
        self._client.close()

    # ------------------------------------------------------------------
    # Internal request helper with auto re-login
    # ------------------------------------------------------------------

    def _request(
        self,
        method: str,
        path: str,
        *,
        params: dict[str, Any] | None = None,
        json_body: dict[str, Any] | None = None,
        _retry: bool = True,
    ) -> Any:
        if not self._logged_in:
            self.login()
        resp = self._client.request(
            method,
            path,
            params=_drop_none(params) if params else None,
            json=json_body,
        )
        # Session expired — re-login and retry once.
        if resp.status_code == 401 and _retry:
            self._logged_in = False
            self.login()
            return self._request(method, path, params=params, json_body=json_body, _retry=False)
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
        container: str | None = None,
        namespace: str | None = None,
        user: str | None = None,
    ) -> dict[str, Any]:
        """POST /api/servers/{id}/allow — grant an authorization scope."""
        body: dict[str, Any] = {"mode": mode}
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
