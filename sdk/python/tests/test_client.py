import unittest
from unittest.mock import patch

import httpx

from gpuardian_sdk.client import GpuardianClient, GpuardianError


class FakeHTTPClient:
    def __init__(self, **kwargs):
        self.headers = kwargs.get("headers", {})
        self.requests = []
        self.next_response = httpx.Response(200, json={"authenticated": True})

    def get(self, path):
        self.requests.append(("GET", path))
        return self.next_response

    def request(self, method, path, params=None, json=None):
        self.requests.append((method, path, params, json))
        return self.next_response

    def close(self):
        pass


class GpuardianClientAuthTests(unittest.TestCase):
    def test_access_token_auth_uses_bearer_and_validates(self):
        with patch("gpuardian_sdk.client.httpx.Client", FakeHTTPClient):
            client = GpuardianClient("https://gateway.example", "ga_secret")
            self.assertEqual(client._client.headers["Authorization"], "Bearer ga_secret")
            client.validate_auth()

    def test_unauthorized_request_reports_invalid_access_token(self):
        with patch("gpuardian_sdk.client.httpx.Client", FakeHTTPClient):
            client = GpuardianClient("https://gateway.example", "ga_secret")
            client._client.next_response = httpx.Response(401, json={"error": "expired"})
            with self.assertRaisesRegex(GpuardianError, "access token is invalid"):
                client.list_servers()

    def test_empty_access_token_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "access_token is required"):
            GpuardianClient("https://gateway.example", "  ")

    def test_error_carries_status_code_and_detail(self):
        with patch("gpuardian_sdk.client.httpx.Client", FakeHTTPClient):
            client = GpuardianClient("https://gateway.example", "ga_secret")
            client._client.next_response = httpx.Response(404, json={"error": "server not found"})
            with self.assertRaises(GpuardianError) as caught:
                client.revoke("srv_missing", "grp_test")
            self.assertEqual(caught.exception.status_code, 404)
            self.assertEqual(caught.exception.detail, "server not found")
            self.assertIn("HTTP 404 POST /api/servers/srv_missing/revoke: server not found", str(caught.exception))


class GpuardianClientRequestTests(unittest.TestCase):
    def setUp(self):
        patcher = patch("gpuardian_sdk.client.httpx.Client", FakeHTTPClient)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.client = GpuardianClient("https://gateway.example", "ga_secret")
        self.http = self.client._client
        self.http.next_response = httpx.Response(200, json={})

    def test_history_search_sends_include_summary_only_when_set(self):
        self.client.history_search(limit=50)
        self.client.history_search(limit=50, cursor="next", include_summary=False)

        first, page = self.http.requests
        self.assertEqual(first[0:2], ("POST", "/api/history/search"))
        self.assertNotIn("include_summary", first[3])
        self.assertEqual(page[3], {"limit": 50, "cursor": "next", "include_summary": False})

    def test_fleet_snapshot_compact_query(self):
        self.client.fleet_snapshot()
        self.client.fleet_snapshot(compact=True)

        full, compact = self.http.requests
        self.assertIsNone(full[2])
        self.assertEqual(compact[2], {"compact": "1"})

    def test_list_keys_keeps_node_sync_by_default(self):
        self.http.next_response = httpx.Response(200, json=[])
        self.client.list_keys()
        self.client.list_keys(include_node_sync=False)

        default, without = self.http.requests
        self.assertEqual(default[0:3], ("GET", "/api/keys", {"include": "node_sync"}))
        self.assertIsNone(without[2])

    def test_history_session_timeline_limit(self):
        self.client.history_session("sess_1")
        self.client.history_session("sess_1", timeline_limit=180)

        default, limited = self.http.requests
        self.assertEqual(default[0:2], ("GET", "/api/history/sessions/sess_1"))
        self.assertFalse(default[2])
        self.assertEqual(limited[2], {"timeline_limit": 180})


if __name__ == "__main__":
    unittest.main()
