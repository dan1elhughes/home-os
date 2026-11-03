import json
from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[1]


class DoctorConfigTests(unittest.TestCase):
    def test_both_networks_have_independent_healthy_workers(self):
        result = subprocess.run(
            ["docker", "compose", "-f", str(ROOT / "docker-compose.yml"),
             "config", "--format", "json"],
            check=True, capture_output=True, text=True,
        )
        services = json.loads(result.stdout)["services"]
        workers = {
            service["environment"]["DOCTOR_NETWORK"]: service
            for service in services.values()
        }
        self.assertEqual(set(workers), {"main", "ingress"})
        for network, service in workers.items():
            with self.subTest(network=network):
                env = service["environment"]
                self.assertEqual(len(env["DOCTOR_NODES"].split()), 3)
                minimum = int(env.get("DOCTOR_MIN_ENDPOINTS", "10"))
                self.assertGreaterEqual(minimum, 3)
                if network == "ingress":
                    self.assertLessEqual(minimum, 9)
                self.assertIn("healthcheck", service)
                self.assertIn("doctor", service["networks"])
                self.assertIn("overlay_doctor_ssh_key", [
                    secret["source"] for secret in service["secrets"]
                ])
                self.assertIn("doctor_reconcile", [
                    config["source"] for config in service["configs"]
                ])


if __name__ == "__main__":
    unittest.main()
