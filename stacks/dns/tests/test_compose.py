import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
DNS = ROOT / "dns"
DEPLOY = ROOT / "deploy.sh"
FILES = ("unbound.conf", "entrypoint.sh", "generator.py")
ENV = dict(os.environ, DNS_CONFIG_HASH="000000000000", NEXTDNS_PROFILE_ID="abc123")


class ComposeTests(unittest.TestCase):
    def test_dns_compose_uses_vip_only_host_network_and_runs_expected_services(self):
        server_config = (DNS / "unbound.conf").read_text()
        self.assertIn("interface: 10.10.10.20", server_config)
        self.assertIn("ip-freebind: yes", server_config)
        self.assertNotIn("interface: 0.0.0.0", server_config)
        result = subprocess.run(
            ["docker", "compose", "-f", str(DNS / "docker-compose.yml"), "config", "--format", "json"],
            env=ENV, capture_output=True, text=True, check=True,
        )
        config = json.loads(result.stdout)
        unbound = config["services"]["unbound"]
        generator = config["services"]["generator"]
        self.assertEqual(unbound["deploy"]["mode"], "global")
        self.assertEqual(generator["deploy"]["replicas"], 1)
        self.assertNotIn("ports", unbound)
        self.assertEqual(list(unbound["networks"]), ["host"])
        self.assertEqual(list(generator["networks"]), ["default"])
        self.assertFalse(generator.get("ports"))
        self.assertFalse(any("docker.sock" in str(m) for m in generator.get("volumes", [])))
        self.assertEqual(unbound["healthcheck"]["test"][0], "CMD-SHELL")
        health = unbound["healthcheck"]["test"][1]
        self.assertIn("unbound-control -c /opt/unbound/etc/unbound/unbound.conf status", health)
        self.assertIn("unbound-control -c /opt/unbound/etc/unbound/unbound.conf list_local_data", health)
        self.assertTrue(health)
        self.assertNotIn("drill @10.10.10.20", health)
        self.assertEqual(generator["environment"]["TRAEFIK_API_URL"], "http://traefik:8080/api/http/routers")
        self.assertEqual(generator["environment"]["RECORDS_PATH"], "/data/hosts.conf")
        self.assertEqual(generator["environment"]["TRAEFIK_IP"], "10.10.10.20")
        self.assertEqual(config["networks"]["default"]["name"], "main")
        self.assertEqual(config["networks"]["host"]["name"], "host")
        self.assertEqual({v["target"]: v.get("read_only", False) for v in unbound["volumes"]}["/opt/unbound/etc/unbound/generated"], True)
        anchor_volume = next(v for v in unbound["volumes"] if v["target"] == "/opt/unbound/etc/unbound/var")
        self.assertEqual(anchor_volume["type"], "volume")
        self.assertFalse(anchor_volume.get("read_only", False))
        self.assertIn(anchor_volume["source"], config["volumes"])
        self.assertEqual(config["volumes"][anchor_volume["source"]]["driver"], "local")
        self.assertEqual({v["target"]: v.get("read_only", False) for v in generator["volumes"]}["/data"], False)
        self.assertEqual({v["name"] for v in config["configs"].values()},
                         {f"dns_{Path(name).stem}_000000000000" for name in FILES})
        self.assertEqual({c["source"] for c in unbound["configs"]}, {"unbound", "entrypoint"})

    def test_health_check_requires_exact_loaded_home_a_record(self):
        result = subprocess.run(
            ["docker", "compose", "-f", str(DNS / "docker-compose.yml"), "config", "--format", "json"],
            env=ENV, capture_output=True, text=True, check=True,
        )
        health = json.loads(result.stdout)["services"]["unbound"]["healthcheck"]["test"][1]
        with tempfile.TemporaryDirectory() as tmp:
            control = Path(tmp) / "unbound-control"
            control.write_text('#!/bin/sh\nif [ "$3" = status ]; then exit 0; fi\ncat "$RECORDS"\n')
            control.chmod(0o755)
            for records, expected in (
                ("home.danhughes.dev. 60 IN A 10.10.10.20\n", 0),
                ("home.danhughes.dev.\t42\tIN\tA\t10.10.10.20\n", 0),
                ("other-home.danhughes.dev. 60 IN A 10.10.10.20\n", 1),
                ("home.danhughes.dev. 60 IN AAAA 10.10.10.20\n", 1),
                ("home.danhughes.dev. 60 IN A 10.10.10.200\n", 1),
                ("home.danhughes.dev. 60 IN A 10.10.10.20.evil\n", 1),
            ):
                with self.subTest(records=records):
                    path = Path(tmp) / "records"
                    path.write_text(records)
                    status = subprocess.run(["sh", "-c", health], env=dict(os.environ, PATH=f"{tmp}:{os.environ['PATH']}", RECORDS=str(path)), capture_output=True)
                    self.assertEqual(status.returncode == 0, expected == 0)

    def test_traefik_api_is_not_published(self):
        result = subprocess.run(
            ["docker", "compose", "-f", str(ROOT / "traefik/docker-compose.yml"), "config", "--format", "json"],
            env=dict(os.environ, CF_API_TOKEN="test"), capture_output=True, text=True, check=True,
        )
        traefik = json.loads(result.stdout)["services"]["traefik"]
        self.assertIn("--api.insecure=true", traefik["command"])
        self.assertIn("--entrypoints.traefik.address=:8080", traefik["command"])
        self.assertEqual({p["target"] for p in traefik["ports"]}, {80, 443})
        self.assertFalse(any("8080" in label for label in traefik.get("deploy", {}).get("labels", {}).values()))


class DeployTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "dns").mkdir()
        for name in FILES:
            shutil.copyfile(DNS / name, self.root / "dns" / name)

    def call(self, command, env=None):
        return subprocess.run(["bash", "-c", f'source "$DEPLOY_SCRIPT"; {command}'],
                              env=dict(os.environ, DEPLOY_SCRIPT=str(DEPLOY), **(env or {})),
                              capture_output=True, text=True, check=True).stdout.strip()

    def test_dns_hash_tracks_all_files_and_is_stable(self):
        original = self.call(f'export_dns_config_hash "{self.root}"; echo "$DNS_CONFIG_HASH"')
        self.assertRegex(original, r"^[a-f0-9]{12}$")
        self.assertEqual(self.call(f'export_dns_config_hash "{self.root}"; echo "$DNS_CONFIG_HASH"'), original)
        for name in FILES:
            with self.subTest(file=name):
                path = self.root / "dns" / name
                contents = path.read_bytes()
                path.write_bytes(contents + b"\n# test change\n")
                changed = self.call(f'export_dns_config_hash "{self.root}"; echo "$DNS_CONFIG_HASH"')
                self.assertNotEqual(changed, original)
                path.write_bytes(contents)
                self.assertEqual(self.call(f'export_dns_config_hash "{self.root}"; echo "$DNS_CONFIG_HASH"'), original)

    def test_hash_is_scoped_to_dns_stack(self):
        self.assertEqual(self.call('DNS_CONFIG_HASH=stale; export_dns_config_hash homeassistant; '
                                   'echo "${DNS_CONFIG_HASH:-unset}"'), "unset")

    def test_prune_only_removes_old_unused_dns_configs(self):
        bin_dir = self.root / "bin"
        bin_dir.mkdir()
        docker = bin_dir / "docker"
        docker.write_text('''#!/bin/sh
if [ "$1" = config ] && [ "$2" = ls ]; then
    printf '%s\\n' dns_unbound_old dns_generator_abcdefabcdef dns_unbound_current predbat_apps_old unrelated
elif [ "$1" = config ] && [ "$2" = rm ]; then
    printf '%s\\n' "$3" >> "$REMOVED"
    [ "$3" != dns_generator_abcdefabcdef ]
fi
''')
        docker.chmod(0o755)
        removed = self.root / "removed"
        self.call('DNS_CONFIG_HASH=current; prune_dns_configs',
                  {"PATH": f"{bin_dir}:{os.environ['PATH']}", "REMOVED": str(removed)})
        self.assertEqual(removed.read_text().splitlines(), ["dns_unbound_old", "dns_generator_abcdefabcdef"])


if __name__ == "__main__":
    unittest.main()
