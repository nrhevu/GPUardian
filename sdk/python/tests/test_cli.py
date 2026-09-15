import io
import json
import unittest
from contextlib import redirect_stderr, redirect_stdout
from unittest.mock import patch

from gpuardian_sdk import cli
from gpuardian_sdk.client import GpuardianError


class FakeClient:
    instances = []

    def __init__(self, base_url, access_token, **kwargs):
        self.base_url = base_url
        self.access_token = access_token
        self.kwargs = kwargs
        self.calls = []
        self.closed = False
        self.servers = [
            {"id": "srv_alpha", "name": "alpha"},
            {"id": "srv_beta", "name": "beta"},
        ]
        self.__class__.instances.append(self)

    def close(self):
        self.closed = True

    def validate_auth(self):
        self.calls.append(("validate_auth",))

    def list_servers(self):
        self.calls.append(("list_servers",))
        return self.servers

    def create_reservation(self, server_id, **kwargs):
        self.calls.append(("create_reservation", server_id, kwargs))
        return {"group_id": "grp_test", "gpus": kwargs["gpus"]}

    def allow(self, server_id, **kwargs):
        self.calls.append(("allow", server_id, kwargs))
        return {"authorization_id": "auth_test", "mode": kwargs["mode"]}


class ErrorClient(FakeClient):
    def list_servers(self):
        raise GpuardianError("access token is invalid")


class CLITests(unittest.TestCase):
    def setUp(self):
        FakeClient.instances = []

    def run_cli(self, argv, client_class=FakeClient, env=None):
        stdout = io.StringIO()
        stderr = io.StringIO()
        environ = {"GPUARDIAN_ACCESS_TOKEN": "ga_test", **(env or {})}
        with patch.dict("os.environ", environ, clear=True), patch(
            "gpuardian_sdk.cli.GpuardianClient", client_class
        ), redirect_stdout(stdout), redirect_stderr(stderr):
            status = cli.main(argv)
        return status, stdout.getvalue(), stderr.getvalue()

    def test_nodes_prints_json_and_closes_client(self):
        status, output, error = self.run_cli(["--url", "https://gateway.example", "nodes"])

        self.assertEqual(status, 0)
        self.assertEqual(error, "")
        self.assertEqual(json.loads(output)[1]["name"], "beta")
        self.assertTrue(FakeClient.instances[0].closed)

    def test_reserve_resolves_node_name_and_parses_gpus(self):
        status, output, error = self.run_cli(
            [
                "--url",
                "https://gateway.example",
                "reserve",
                "--node",
                "beta",
                "--gpus",
                "2,3",
                "--purpose",
                "training",
                "--ttl",
                "2h",
            ]
        )

        self.assertEqual(status, 0)
        self.assertEqual(error, "")
        self.assertEqual(json.loads(output)["group_id"], "grp_test")
        call = FakeClient.instances[0].calls[-1]
        self.assertEqual(call[0:2], ("create_reservation", "srv_beta"))
        self.assertEqual(call[2]["gpus"], [2, 3])
        self.assertEqual(call[2]["purpose"], "training")

    def test_allow_dispatches_selected_scope(self):
        status, output, error = self.run_cli(
            [
                "--url",
                "https://gateway.example",
                "allow",
                "k8s",
                "--node",
                "srv_alpha",
                "--namespace",
                "training",
                "--run-name",
                "nightly",
            ]
        )

        self.assertEqual(status, 0)
        self.assertEqual(error, "")
        self.assertEqual(json.loads(output)["mode"], "k8s")
        call = FakeClient.instances[0].calls[-1]
        self.assertEqual(call[0:2], ("allow", "srv_alpha"))
        self.assertEqual(call[2]["namespace"], "training")
        self.assertEqual(call[2]["run_name"], "nightly")

    def test_missing_token_is_a_configuration_error(self):
        status, output, error = self.run_cli(
            ["--url", "https://gateway.example", "nodes"], env={"GPUARDIAN_ACCESS_TOKEN": ""}
        )

        self.assertEqual(status, 2)
        self.assertEqual(output, "")
        self.assertIn("GPUARDIAN_ACCESS_TOKEN is required", error)
        self.assertEqual(FakeClient.instances, [])

    def test_sdk_error_returns_one_and_closes_client(self):
        status, output, error = self.run_cli(
            ["--url", "https://gateway.example", "nodes"], client_class=ErrorClient
        )

        self.assertEqual(status, 1)
        self.assertEqual(output, "")
        self.assertIn("access token is invalid", error)
        self.assertTrue(ErrorClient.instances[0].closed)


if __name__ == "__main__":
    unittest.main()
