import unittest
from unittest.mock import patch

import httpx

from gpuardian_mcp.client import GpuardianClient, GpuardianError


class FakeHTTPClient:
    def __init__(self, **kwargs):
        self.headers = kwargs.get("headers", {})
        self.posts = []
        self.requests = []
        self.next_response = httpx.Response(200, json={"authenticated": True})

    def get(self, path):
        self.requests.append(("GET", path))
        return self.next_response

    def post(self, path, json=None):
        self.posts.append((path, json))
        return self.next_response

    def request(self, method, path, params=None, json=None):
        self.requests.append((method, path, params, json))
        return self.next_response

    def close(self):
        pass


class GpuardianClientAuthTests(unittest.TestCase):
    def test_token_auth_uses_bearer_and_validates_without_password(self):
        with patch("gpuardian_mcp.client.httpx.Client", FakeHTTPClient):
            client = GpuardianClient("https://gateway.example", token="ga_secret")
            self.assertEqual(client._client.headers["Authorization"], "Bearer ga_secret")
            client.validate_auth()
            self.assertEqual(client._client.posts, [])

    def test_password_is_discarded_and_not_reused_after_401(self):
        with patch("gpuardian_mcp.client.httpx.Client", FakeHTTPClient):
            client = GpuardianClient(
                "https://gateway.example", username="alice", password="secret-password"
            )
            client.validate_auth()
            self.assertIsNone(client._password)
            self.assertEqual(len(client._client.posts), 1)

            client._client.next_response = httpx.Response(401, json={"error": "expired"})
            with self.assertRaisesRegex(GpuardianError, "session expired"):
                client.list_servers()
            self.assertEqual(len(client._client.posts), 1)


if __name__ == "__main__":
    unittest.main()
