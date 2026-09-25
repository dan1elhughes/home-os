import io
import json
import os
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest.mock import patch

from generator import fetch_routers, hosts_from_routers, poll, publish, render_hosts


class GeneratorTests(unittest.TestCase):
    def test_extracts_enabled_exact_hosts_from_compound_rules(self):
        routers = [
            {"status": "enabled", "rule": "Host(`home.danhughes.dev`)"},
            {
                "status": "enabled",
                "rule": "Host(`a.danhughes.dev`, `b.danhughes.dev`) && PathPrefix(`/x`)"},
        ]

        self.assertEqual(
            hosts_from_routers(routers),
            {"home.danhughes.dev", "a.danhughes.dev", "b.danhughes.dev"},
        )

    def test_extracts_all_exact_host_calls_in_or_rule(self):
        routers = [{
            "status": "enabled",
            "rule": "Host(`home.danhughes.dev`) || Host(`second.danhughes.dev`, `third.danhughes.dev`) || HostRegexp(`{name:.+}.danhughes.dev`)",
        }]

        self.assertEqual(
            hosts_from_routers(routers),
            {"home.danhughes.dev", "second.danhughes.dev", "third.danhughes.dev"},
        )

    def test_ignores_inactive_unsupported_and_invalid_hosts(self):
        routers = [
            {"status": "disabled", "rule": "Host(`off.danhughes.dev`)"},
            {"status": "enabled", "rule": "HostRegexp(`{name:.+}.danhughes.dev`)"},
            {"status": "enabled", "rule": "Host(`other.example`)"},
            {"status": "enabled", "rule": "Host(`bad_name.danhughes.dev`)"},
        ]

        self.assertEqual(hosts_from_routers(routers), set())

    def test_warning_router_with_exact_host_is_included(self):
        self.assertEqual(hosts_from_routers([
            {"status": "warning", "rule": "Host(`home.danhughes.dev`)"},
            {"status": "disabled", "rule": "Host(`off.danhughes.dev`)"},
            {"status": "warning", "rule": "HostRegexp(`{name:.+}.danhughes.dev`)"},
        ]), {"home.danhughes.dev"})

    def test_mixed_exact_and_regexp_rule_warns(self):
        with self.assertLogs("generator", "WARNING") as logs:
            hosts = hosts_from_routers([{
                "status": "warning",
                "rule": "Host(`home.danhughes.dev`) || HostRegexp(`{name:.+}.danhughes.dev`)",
            }])
        self.assertEqual(hosts, {"home.danhughes.dev"})
        self.assertIn("HostRegexp", " ".join(logs.output))

    def test_normalizes_case_and_trailing_dot(self):
        routers = [{"status": "enabled", "rule": "Host(`HOME.DanHughes.Dev.`)"}]

        self.assertEqual(hosts_from_routers(routers), {"home.danhughes.dev"})

    def test_renders_sorted_static_zone_and_a_record(self):
        self.assertEqual(
            render_hosts({"b.danhughes.dev", "a.danhughes.dev"}, "10.10.10.20"),
            'local-zone: "a.danhughes.dev." static\n'
            'local-data: "a.danhughes.dev. 60 IN A 10.10.10.20"\n'
            'local-zone: "b.danhughes.dev." static\n'
            'local-data: "b.danhughes.dev. 60 IN A 10.10.10.20"\n',
        )

    def test_publish_keeps_inode_when_content_is_unchanged(self):
        with TemporaryDirectory() as directory:
            path = Path(directory) / "hosts.conf"
            path.write_text("working\n")
            inode = path.stat().st_ino

            self.assertFalse(publish(path, "working\n"))
            self.assertEqual(path.stat().st_ino, inode)
            self.assertEqual(path.read_text(), "working\n")

    def test_publish_repairs_unreadable_mode_without_replacing_unchanged_content(self):
        with TemporaryDirectory() as directory:
            path = Path(directory) / "hosts.conf"
            path.write_text("working\n")
            os.chmod(path, 0o600)
            inode = path.stat().st_ino

            self.assertTrue(publish(path, "working\n"))
            self.assertEqual(path.stat().st_mode & 0o777, 0o644)
            self.assertEqual(path.stat().st_ino, inode)
            self.assertEqual(path.read_text(), "working\n")

    def test_publish_replaces_content_with_world_readable_file(self):
        with TemporaryDirectory() as directory:
            path = Path(directory) / "hosts.conf"

            self.assertTrue(publish(path, "new\n"))
            self.assertEqual(path.read_text(), "new\n")
            self.assertEqual(path.stat().st_mode & 0o777, 0o644)

    def test_publish_keeps_old_content_when_replace_fails(self):
        with TemporaryDirectory() as directory:
            path = Path(directory) / "hosts.conf"
            path.write_text("working\n")

            with self.assertLogs("generator", "ERROR") as logs, patch(
                "generator.os.replace", side_effect=OSError("disk error")
            ):
                self.assertFalse(publish(path, "new\n"))

            self.assertEqual(path.read_text(), "working\n")
            self.assertIn("disk error", logs.output[0])

    def test_fetch_routers_rejects_non_router_json(self):
        with patch("generator.urllib.request.urlopen", return_value=io.StringIO("{}")):
            with self.assertRaisesRegex(ValueError, "array of objects"):
                fetch_routers("http://traefik/api/http/routers")

    def test_poll_publishes_valid_snapshot_and_keeps_records_after_error(self):
        valid_response = io.StringIO(
            '[{"status": "enabled", "rule": "Host(`home.danhughes.dev`)"}]'
        )
        with TemporaryDirectory() as directory:
            path = Path(directory) / "hosts.conf"
            with patch(
                "generator.urllib.request.urlopen",
                side_effect=[valid_response, OSError("temporary API error")],
            ) as urlopen, patch("generator.time.sleep") as sleep:
                poll("http://traefik/api/http/routers", path, "10.10.10.20", iterations=2)

            self.assertEqual(urlopen.call_args_list[0].args, ("http://traefik/api/http/routers",))
            self.assertEqual(urlopen.call_args_list[0].kwargs, {"timeout": 5})
            self.assertEqual(sleep.call_args_list, [unittest.mock.call(60), unittest.mock.call(60)])
            self.assertIn('local-data: "home.danhughes.dev. 60 IN A 10.10.10.20"', path.read_text())

    def test_poll_keeps_existing_records_for_empty_or_incomplete_snapshot(self):
        responses = [io.StringIO("[]"), io.StringIO('[{"status": "enabled", "rule": "Host(`a.danhughes.dev`)"}]')]
        with TemporaryDirectory() as directory:
            path = Path(directory) / "hosts.conf"
            path.write_text("working\n")
            with patch("generator.urllib.request.urlopen", side_effect=responses), patch(
                "generator.time.sleep"
            ):
                poll("http://traefik/api/http/routers", path, "10.10.10.20", iterations=2)

            self.assertEqual(path.read_text(), "working\n")

    def test_poll_rejects_large_contraction_then_accepts_one_valid_removal(self):
        initial = {"home.danhughes.dev"} | {f"host{i}.danhughes.dev" for i in range(19)}
        after_removal = initial - {"host18.danhughes.dev"}
        def response(hosts):
            return io.StringIO(json.dumps([
                {"status": "enabled", "rule": f"Host(`{host}`)"} for host in sorted(hosts)
            ]))

        with TemporaryDirectory() as directory:
            path = Path(directory) / "hosts.conf"
            path.write_text(render_hosts(initial, "10.10.10.20"))
            with patch("generator.urllib.request.urlopen", side_effect=[
                response({"home.danhughes.dev"}), response(after_removal)
            ]), patch("generator.time.sleep"):
                with self.assertLogs("generator", "ERROR") as logs:
                    poll("http://traefik/api/http/routers", path, "10.10.10.20", iterations=1)
                self.assertEqual(path.read_text(), render_hosts(initial, "10.10.10.20"))
                poll("http://traefik/api/http/routers", path, "10.10.10.20", iterations=1)
            self.assertIn("contraction", " ".join(logs.output))
            self.assertEqual(path.read_text(), render_hosts(after_removal, "10.10.10.20"))


if __name__ == "__main__":
    unittest.main()
