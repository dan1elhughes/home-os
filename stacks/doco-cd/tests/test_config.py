from pathlib import Path
import unittest

import yaml


class RegistryAuthConfigTests(unittest.TestCase):
    def test_registry_auth_is_mounted_as_a_restricted_external_secret(self):
        path = Path(__file__).resolve().parents[1] / "docker-compose.yml"
        compose = yaml.safe_load(path.read_text())
        service = compose["services"]["doco-cd"]
        mounts = [
            secret
            for secret in service["secrets"]
            if isinstance(secret, dict)
            and secret.get("source") == "doco_cd_registry_auth"
        ]
        self.assertEqual(len(mounts), 1)
        self.assertEqual(mounts[0]["target"], "/root/.docker/config.json")
        self.assertEqual(mounts[0]["mode"], 0o400)
        self.assertTrue(compose["secrets"]["doco_cd_registry_auth"]["external"])
        self.assertNotIn("DOCKER_AUTH_CONFIG", service["environment"])


if __name__ == "__main__":
    unittest.main()
