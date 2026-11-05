import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SANDBOX = "1-abcdefghij"
MACS = ["02:42:0a:00:00:01", "02:42:0a:00:00:02", "02:42:0a:00:00:03"]

FAKE = r'''
import json
import os
from pathlib import Path
import subprocess
import sys

command = Path(sys.argv[0]).name
args = sys.argv[1:]
fixtures = json.loads(Path(os.environ["FIXTURES"]).read_text())
if command == "ssh":
    target = next(i for i, arg in enumerate(args) if arg.startswith("core@"))
    node = args[target].split("@", 1)[1]
    if fixtures[node].get("unreachable"):
        sys.exit(255)
    os.environ["FAKE_NODE"] = node
    sys.exit(subprocess.call(args[target + 1:]))

node = os.environ["FAKE_NODE"]
fixture = fixtures[node]
if command == "docker":
    assert args[:4] == ["network", "inspect", "main", "--format"], args
    if args[4] == "{{.Id}}":
        if fixture.get("id_error"):
            sys.exit(1)
        print("abcdefghij0123456789")
    else:
        assert args[4] == '{{range .Containers}}{{.IPv4Address}} {{.MacAddress}}{{"\\n"}}{{end}}', args
        if fixture.get("inspect_error"):
            sys.exit(1)
        if "endpoints_after_gather" in fixture:
            marker = Path(os.environ["FIXTURES"] + node)
            if marker.exists():
                print(fixture["endpoints_after_gather"])
                sys.exit(0)
            marker.touch()
        print(fixture.get("endpoints", ""))
elif command == "sudo":
    if args == ["ls", "/var/run/docker/netns"]:
        print(fixture["sandboxes"])
        if fixture.get("list_error"):
            sys.exit(1)
    else:
        assert args[:3] == ["nsenter", "--net=/var/run/docker/netns/1-abcdefghij", "bridge"], args
        if args[3:] == ["fdb", "show"]:
            if fixture.get("fdb_error"):
                sys.exit(1)
            print(fixture.get("fdb", ""))
        else:
            assert args[3] == "fdb" and args[4] in ("del", "replace"), args
            if args[6:] == ["dev", "vxlan0", "master"]:
                assert args[4] == "del", args
                with open(os.environ["APPLIED"], "a") as output:
                    output.write(f"{node} del-master {args[5]}\n")
                sys.exit(0)
            assert args[6:9] == ["dev", "vxlan0", "dst"], args
            assert args[10:] == ["self", "permanent"], args
            with open(os.environ["APPLIED"], "a") as output:
                output.write(f"{node} {args[4]} {args[5]} {args[9]}\n")
else:
    raise AssertionError(command)
'''


class ReconcileTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.work = Path(self.temp.name)
        for command in ("ssh", "docker", "sudo"):
            path = self.work / command
            path.write_text(f"#!{sys.executable}\n" + FAKE)
            path.chmod(0o755)

    def run_cycle(self, nodes, minimum=2, dry_run=False):
        fixtures = self.work / "fixtures.json"
        fixtures.write_text(json.dumps(dict(zip(("10.0.0.1", "10.0.0.2", "10.0.0.3"), nodes))))
        applied = self.work / "applied"
        beat = self.work / "beat"
        applied.unlink(missing_ok=True)
        beat.unlink(missing_ok=True)
        result = subprocess.run(
            ["sh", str(ROOT / "reconcile.sh")], capture_output=True, text=True,
            env={**os.environ, "PATH": f"{self.work}:{os.environ['PATH']}",
                 "FIXTURES": str(fixtures), "APPLIED": str(applied),
                 "TMPDIR": str(self.work), "DOCTOR_ONCE": "1", "DOCTOR_NETWORK": "main",
                 "DOCTOR_NODES": "cl01,10.0.0.1 cl02,10.0.0.2 cl03,10.0.0.3",
                  "DOCTOR_MIN_ENDPOINTS": str(minimum), "DOCTOR_DRY_RUN": str(int(dry_run)),
                 "DOCTOR_KEY": str(self.work / "absent-key"), "DOCTOR_BEAT_FILE": str(beat)},
        )
        writes = applied.read_text().splitlines() if applied.exists() else []
        return result, writes, beat.exists()

    def active(self, index=0):
        return {"sandboxes": f"ingress_sbox\n{SANDBOX}",
                "endpoints": f"10.1.0.{index + 1}/24 {MACS[index]}"}

    def empty(self):
        return {"sandboxes": "ingress_sbox\n1-hpxgg1bni4", "endpoints": ""}

    def test_empty_nodes_allow_active_node_repairs(self):
        active = self.active()
        active["endpoints"] += f"\n10.1.0.2/24 {MACS[1]}"
        active["fdb"] = f"{MACS[0]} dev vxlan0 dst 10.0.0.2 self permanent"
        result, writes, beat = self.run_cycle([active, self.empty(), self.empty()])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(writes, [f"10.0.0.1 del {MACS[0]} 10.0.0.2"])
        self.assertTrue(beat)
        self.assertIn("cycle nodes=3 endpoints=2", result.stdout)

    def test_gather_errors_stop_all_repairs(self):
        cases = {
            "unreachable": {**self.empty(), "unreachable": True},
            "id_error": {**self.empty(), "id_error": True},
            "inspect_error_empty": {**self.empty(), "inspect_error": True},
            "inspect_error_active": {**self.active(1), "inspect_error": True},
            "list_error_empty": {**self.empty(), "list_error": True},
            "list_error_with_output": {**self.active(1), "list_error": True},
            "missing_with_endpoints": {**self.active(1), "sandboxes": "ingress_sbox"},
            "missing_with_nonstandard_mac": {**self.empty(), "endpoints": "10.1.0.2/24 aa:bb:cc:dd:ee:ff"},
            "multiple_empty": {**self.empty(), "sandboxes": "1-abcdefghij\n2-abcdefghij"},
            "multiple_active": {**self.active(1), "sandboxes": "1-abcdefghij\n2-abcdefghij"},
            "fdb_error": {**self.active(1), "fdb_error": True},
        }
        for name, broken in cases.items():
            with self.subTest(name=name):
                result, writes, beat = self.run_cycle([self.active(), broken, self.active(2)])
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertEqual(writes, [])
                self.assertFalse(beat)
                self.assertIn("skip cycle: gathered 2 of 3 nodes", result.stdout)

    def test_empty_nodes_do_not_bypass_minimum(self):
        result, writes, beat = self.run_cycle([self.active(), self.empty(), self.empty()])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("skip cycle: 1 endpoints is below the 2 minimum", result.stdout)
        self.assertEqual(writes, [])
        self.assertFalse(beat)

    def test_local_endpoint_wrongly_forwarded_to_vxlan_is_relearned(self):
        nodes = [self.active(i) for i in range(3)]
        nodes[0]["fdb"] = (
            f"{MACS[0]} dev vxlan0 master br0\n"
            f"{MACS[1]} dev vxlan0 master br0\n"
            f"{MACS[1]} dev vxlan0 dst 10.0.0.2 self permanent\n"
            f"{MACS[2]} dev veth99 master br0\n"
            f"{MACS[2]} dev vxlan0 dst 10.0.0.3 self permanent\n"
            "02:42:ff:ff:ff:ff dev vxlan0 master br0"
        )
        result, writes, beat = self.run_cycle(nodes)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(f"10.0.0.1 del-master {MACS[0]}", writes)
        self.assertEqual([row for row in writes if "del-master" in row],
                         [f"10.0.0.1 del-master {MACS[0]}"])
        self.assertTrue(beat)

    def test_correct_local_bridge_port_is_left_unchanged(self):
        nodes = [self.active(i) for i in range(3)]
        nodes[0]["fdb"] = f"{MACS[0]} dev veth95 master br0"
        result, writes, beat = self.run_cycle(nodes)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse(any("del-master" in row for row in writes))
        self.assertTrue(beat)

    def test_master_repair_rechecks_local_endpoint_ownership(self):
        nodes = [self.active(i) for i in range(3)]
        nodes[0]["fdb"] = f"{MACS[0]} dev vxlan0 master br0"
        nodes[0]["endpoints_after_gather"] = ""
        result, writes, beat = self.run_cycle(nodes)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any("del-master" in row for row in writes))
        self.assertFalse(beat)
        self.assertIn("apply failed node=cl01 action=del-master", result.stdout)

    def test_master_repair_dry_run_does_not_write(self):
        nodes = [self.active(i) for i in range(3)]
        nodes[0]["fdb"] = f"{MACS[0]} dev vxlan0 master br0"
        result, writes, beat = self.run_cycle(nodes, dry_run=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(writes, [])
        self.assertTrue(beat)
        self.assertIn(f"would del-master mac={MACS[0]}", result.stdout)

    def test_regular_nodes_repair_missing_wrong_and_local_entries(self):
        nodes = [self.active(i) for i in range(3)]
        nodes[0]["fdb"] = (
            f"{MACS[0]} dev vxlan0 dst 10.0.0.2 self permanent\n"
            f"{MACS[1]} dev vxlan0 dst 10.0.0.3 self permanent\n"
            "02:42:ff:ff:ff:ff dev vxlan0 dst 10.0.0.3 self permanent"
        )
        result, writes, beat = self.run_cycle(nodes)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(set(writes), {
            f"10.0.0.1 del {MACS[0]} 10.0.0.2",
            f"10.0.0.1 replace {MACS[1]} 10.0.0.2",
            f"10.0.0.1 replace {MACS[2]} 10.0.0.3",
            f"10.0.0.2 replace {MACS[0]} 10.0.0.1",
            f"10.0.0.2 replace {MACS[2]} 10.0.0.3",
            f"10.0.0.3 replace {MACS[0]} 10.0.0.1",
            f"10.0.0.3 replace {MACS[1]} 10.0.0.2",
        })
        self.assertTrue(beat)
        self.assertIn("conflicts=0 unknown=1 fixes=7", result.stdout)


if __name__ == "__main__":
    unittest.main()
