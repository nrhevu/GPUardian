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


if __name__ == "__main__":
    unittest.main()
