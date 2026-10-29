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
            {"id": "srv_0123456789abcdef", "name": "gamma"},
            {"id": "srv_delta", "name": "srv_fedcba9876543210"},
        ]
        self.__class__.instances.append(self)

    def close(self):
        self.closed = True

    def validate_auth(self):
        self.calls.append(("validate_auth",))

    def list_servers(self):
        self.calls.append(("list_servers",))
        return self.servers

    def _require_server(self, server_id):
        # Mirrors the gateway: unknown node IDs fail before any side effect.
        if not any(server["id"] == server_id for server in self.servers):
            raise GpuardianError(
                "HTTP 404 POST /api/servers/x: server not found",
                status_code=404,
                detail="server not found",
            )

    def create_reservation(self, server_id, **kwargs):
        self.calls.append(("create_reservation", server_id, kwargs))
        self._require_server(server_id)
        return {"group_id": "grp_test", "gpus": kwargs["gpus"]}

    def allow(self, server_id, **kwargs):
        self.calls.append(("allow", server_id, kwargs))
        self._require_server(server_id)
        return {"authorization_id": "auth_test", "mode": kwargs["mode"]}

    def revoke(self, server_id, id):
        self.calls.append(("revoke", server_id, id))
        self._require_server(server_id)
        return {"revoked": id}

    def fleet_snapshot(self, **kwargs):
        self.calls.append(("fleet_snapshot", kwargs))
        return {"servers": []}

    def history_search(self, **kwargs):
        self.calls.append(("history_search", kwargs))
        return {"sessions": [], "next_cursor": ""}

    def history_session(self, session_id, **kwargs):
        self.calls.append(("history_session", session_id, kwargs))
        return {"id": session_id}


class ForbiddenRevokeClient(FakeClient):
    def revoke(self, server_id, id):
        self.calls.append(("revoke", server_id, id))
        raise GpuardianError(
            "HTTP 403 POST /api/servers/x/revoke: revoke access denied",
            status_code=403,
            detail="revoke access denied",
        )


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

    def test_generated_node_id_skips_server_list(self):
        status, output, error = self.run_cli(
            [
                "--url",
                "https://gateway.example",
                "reserve",
                "--node",
                "srv_0123456789abcdef",
                "--gpus",
                "0",
            ]
        )

        self.assertEqual(status, 0)
        self.assertEqual(error, "")
        self.assertEqual(json.loads(output)["group_id"], "grp_test")
        calls = FakeClient.instances[0].calls
        self.assertEqual([call[0:2] for call in calls], [("create_reservation", "srv_0123456789abcdef")])

    def test_id_shaped_node_name_falls_back_to_name_resolution(self):
        status, output, error = self.run_cli(
            [
                "--url",
                "https://gateway.example",
                "revoke",
                "--node",
                "srv_fedcba9876543210",
                "--id",
                "grp_test",
            ]
        )

        self.assertEqual(status, 0)
        self.assertEqual(error, "")
        self.assertEqual(json.loads(output), {"revoked": "grp_test"})
        calls = FakeClient.instances[0].calls
        self.assertEqual(
            [call[0:2] for call in calls],
            [("revoke", "srv_fedcba9876543210"), ("list_servers",), ("revoke", "srv_delta")],
        )

    def test_unknown_generated_node_id_reports_node_not_found(self):
        status, output, error = self.run_cli(
            [
                "--url",
                "https://gateway.example",
                "allow",
                "user",
                "--node",
                "srv_aaaaaaaaaaaaaaaa",
                "--user",
                "alice",
            ]
        )

        self.assertEqual(status, 1)
        self.assertEqual(output, "")
        self.assertIn("node not found: srv_aaaaaaaaaaaaaaaa", error)
        calls = FakeClient.instances[0].calls
        self.assertEqual([call[0] for call in calls], ["allow", "list_servers"])

    def test_generated_node_id_other_errors_do_not_fall_back(self):
        status, output, error = self.run_cli(
            [
                "--url",
                "https://gateway.example",
                "revoke",
                "--node",
                "srv_0123456789abcdef",
                "--id",
                "grp_test",
            ],
            client_class=ForbiddenRevokeClient,
        )

        self.assertEqual(status, 1)
        self.assertEqual(output, "")
        self.assertIn("revoke access denied", error)
        calls = ForbiddenRevokeClient.instances[0].calls
        self.assertEqual([call[0] for call in calls], ["revoke"])

    def test_query_option_flags_reach_the_client(self):
        self.run_cli(["--url", "https://gateway.example", "fleet", "--compact"])
        self.run_cli(["--url", "https://gateway.example", "history", "search", "--cursor", "c1", "--no-summary"])
        self.run_cli(["--url", "https://gateway.example", "history", "search"])
        self.run_cli(["--url", "https://gateway.example", "history", "session", "--id", "sess_1", "--timeline-limit", "180"])

        fleet, search_page, search_first, session = (instance.calls[-1] for instance in FakeClient.instances)
        self.assertEqual(fleet, ("fleet_snapshot", {"compact": True}))
        self.assertIs(search_page[1]["include_summary"], False)
        self.assertEqual(search_page[1]["cursor"], "c1")
        self.assertIsNone(search_first[1]["include_summary"])
        self.assertEqual(session, ("history_session", "sess_1", {"timeline_limit": 180}))

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
