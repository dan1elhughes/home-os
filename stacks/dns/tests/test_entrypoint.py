import os
import subprocess
import tempfile
import time
import unittest
from pathlib import Path


DNS = Path(__file__).resolve().parents[1]
SCRIPT = DNS / "entrypoint.sh"
HOME = 'local-zone: "home.danhughes.dev." static\nlocal-data: "home.danhughes.dev. 60 IN A 10.10.10.20"\n'
GENERATED = HOME + 'local-zone: "new.danhughes.dev." static\nlocal-data: "new.danhughes.dev. 60 IN A 10.10.10.20"\n'
UPDATED = HOME + 'local-zone: "updated.danhughes.dev." static\nlocal-data: "updated.danhughes.dev. 60 IN A 10.10.10.20"\n'


class EntrypointTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.runtime = self.root / "runtime"
        self.generated = self.root / "generated.conf"
        self.events = self.root / "events"
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.write_command("unbound-checkconf", '''#!/bin/sh
grep -q 'INVALID' "$DNS_RUNTIME_DIR/hosts.conf" && exit 1
grep -q 'forward-tls-upstream: yes' "$DNS_RUNTIME_DIR/forward.conf" || exit 1
exit 0
''')
        self.write_command("unbound-control", '''#!/bin/sh
echo "$*" >> "$DNS_EVENTS"
''')
        self.server = self.root / "server.sh"
        self.server.write_text('''#!/bin/sh
echo started >> "$DNS_EVENTS"
trap 'exit 0' TERM
while :; do sleep 1 & wait $!; done
''')
        self.server.chmod(0o755)
        self.env = dict(os.environ, NEXTDNS_PROFILE_ID="abc123", DNS_RUNTIME_DIR=str(self.runtime),
                         DNS_GENERATED_FILE=str(self.generated),
                         DNS_UNBOUND_SCRIPT=str(self.server), DNS_CONFIG_FILE=str(self.root / "unbound.conf"),
                         DNS_POLL_INTERVAL="1", DNS_STARTUP_ATTEMPTS="3", DNS_STARTUP_INTERVAL="0.2",
                         DNS_EVENTS=str(self.events), PATH=f"{self.bin}:{os.environ['PATH']}")

    def write_command(self, name, text):
        path = self.bin / name
        path.write_text(text)
        path.chmod(0o755)

    def start(self):
        proc = subprocess.Popen(["sh", str(SCRIPT)], env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.addCleanup(self.stop, proc)
        return proc

    def stop(self, proc):
        if proc.poll() is None:
            proc.terminate()
        try:
            proc.communicate(timeout=4)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.communicate(timeout=4)

    def until(self, condition):
        for _ in range(50):
            if condition():
                return
            time.sleep(0.1)
        self.fail("entrypoint did not reach expected state")

    def test_rejects_empty_non_alphanumeric_and_wrong_length_profiles(self):
        for profile in ("", "ab!123", "abc12", "abcdef7"):
            with self.subTest(profile=profile):
                self.env["NEXTDNS_PROFILE_ID"] = profile
                result = subprocess.run(["sh", str(SCRIPT)], env=self.env, capture_output=True, timeout=3)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn(profile.encode() if profile else b"abc123", result.stderr)
                self.assertFalse((self.runtime / "forward.conf").exists())

    def test_six_character_profile_renders_tls_forwarders_from_generated_file(self):
        self.generated.write_text(GENERATED)
        proc = self.start()
        self.until(lambda: self.events.exists() and "started" in self.events.read_text())
        self.assertIsNone(proc.poll())
        self.assertEqual((self.runtime / "hosts.conf").read_text(), GENERATED)
        self.assertEqual((self.runtime / "forward.conf").read_text(), '''forward-zone:
    name: "."
    forward-tls-upstream: yes
    forward-first: no
    forward-addr: 45.90.28.0@853#abc123.dns.nextdns.io
    forward-addr: 45.90.30.0@853#abc123.dns.nextdns.io
''')

    def test_waits_for_generated_file_before_starting(self):
        proc = self.start()
        self.until(lambda: (self.runtime / "forward.conf").exists())
        self.assertIsNone(proc.poll())
        self.assertFalse(self.events.exists())
        self.generated.write_text(GENERATED)
        self.until(lambda: self.events.exists() and "started" in self.events.read_text())
        self.assertEqual((self.runtime / "hosts.conf").read_text(), GENERATED)

    def test_missing_or_empty_generated_file_fails_after_bounded_wait(self):
        for contents in (None, ""):
            with self.subTest(contents=contents):
                if contents is None:
                    self.generated.unlink(missing_ok=True)
                else:
                    self.generated.write_text(contents)
                result = subprocess.run(["sh", str(SCRIPT)], env=self.env, capture_output=True, timeout=3)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(b"DNS records", result.stderr)
                self.assertFalse(self.events.exists())

    def test_invalid_startup_attempt_count_fails_before_launch(self):
        self.generated.write_text(GENERATED)
        self.server.write_text('#!/bin/sh\necho started >> "$DNS_EVENTS"\nexit 0\n')
        for count in ("0", "-1", "abc", "1.5"):
            with self.subTest(count=count):
                self.env["DNS_STARTUP_ATTEMPTS"] = count
                result = subprocess.run(["sh", str(SCRIPT)], env=self.env, capture_output=True, timeout=3)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(b"Invalid DNS startup attempts", result.stderr)
                self.assertFalse(self.events.exists())

    def test_startup_rejects_records_without_exact_home_a_record(self):
        self.server.write_text('#!/bin/sh\necho started >> "$DNS_EVENTS"\nexit 0\n')
        for contents in (
            'local-zone: "other.danhughes.dev." static\nlocal-data: "other.danhughes.dev. 60 IN A 10.10.10.20"\n',
            'local-zone: "home.danhughes.dev." static\nlocal-data: "home.danhughes.dev. 60 IN A 10.10.10.21"\n',
            'local-zone: "home.danhughes.dev." static\nlocal-data: "home.danhughes.dev. 60 IN AAAA ::1"\n',
            '# local-data: "home.danhughes.dev. 60 IN A 10.10.10.20"\n',
        ):
            with self.subTest(contents=contents):
                self.generated.write_text(contents)
                result = subprocess.run(["sh", str(SCRIPT)], env=self.env, capture_output=True, timeout=3)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(b"DNS records", result.stderr)
                self.assertFalse(self.events.exists())

    def test_invalid_initial_records_fail_without_starting_unbound(self):
        self.generated.write_text("INVALID\n")
        result = subprocess.run(["sh", str(SCRIPT)], env=self.env, capture_output=True, timeout=3)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"DNS records", result.stderr)
        self.assertFalse(self.events.exists())

    def test_valid_generated_file_reloads_only_after_full_config_check(self):
        self.generated.write_text(GENERATED)
        proc = self.start()
        self.until(lambda: self.events.exists() and "started" in self.events.read_text())
        self.generated.write_text(UPDATED)
        self.until(lambda: "reload_keep_cache" in self.events.read_text())
        self.assertIsNone(proc.poll())
        self.assertEqual((self.runtime / "hosts.conf").read_text(), UPDATED)
        self.assertEqual(self.events.read_text().splitlines(),
                         ["started", f"-c {self.root / 'unbound.conf'} reload_keep_cache"])
        self.generated.write_text(UPDATED)
        time.sleep(1.5)
        self.assertEqual(len(self.events.read_text().splitlines()), 2)

    def test_invalid_generated_file_keeps_last_good_hosts_and_does_not_reload(self):
        self.generated.write_text(GENERATED)
        proc = self.start()
        self.until(lambda: self.events.exists() and "started" in self.events.read_text())
        initial_events = self.events.read_text() if self.events.exists() else ""
        self.generated.write_text("INVALID\n")
        time.sleep(2.5)
        self.assertIsNone(proc.poll())
        self.assertEqual((self.runtime / "hosts.conf").read_text(), GENERATED)
        self.assertEqual(self.events.read_text() if self.events.exists() else "", initial_events)

    def test_generated_update_without_home_keeps_running_records(self):
        self.generated.write_text(GENERATED)
        proc = self.start()
        self.until(lambda: self.events.exists() and "started" in self.events.read_text())
        self.generated.write_text('local-zone: "other.danhughes.dev." static\nlocal-data: "other.danhughes.dev. 60 IN A 10.10.10.20"\n')
        time.sleep(1.5)
        self.assertIsNone(proc.poll())
        self.assertEqual((self.runtime / "hosts.conf").read_text(), GENERATED)
        self.assertEqual(self.events.read_text().splitlines(), ["started"])

    def test_missing_or_empty_generated_file_keeps_running_records(self):
        self.generated.write_text(GENERATED)
        proc = self.start()
        self.until(lambda: self.events.exists() and "started" in self.events.read_text())
        self.generated.unlink()
        time.sleep(1.2)
        self.generated.write_text("")
        time.sleep(1.2)
        self.assertIsNone(proc.poll())
        self.assertEqual((self.runtime / "hosts.conf").read_text(), GENERATED)
        self.assertEqual(self.events.read_text().splitlines(), ["started"])

    def test_failed_reload_restores_runtime_then_rechecks_and_retries_same_candidate(self):
        self.write_command("unbound-checkconf", '''#!/bin/sh
[ "$1" = "$DNS_CONFIG_FILE" ] || exit 1
grep -q 'forward-tls-upstream: yes' "$DNS_RUNTIME_DIR/forward.conf" || exit 1
if grep -q 'updated.danhughes.dev' "$DNS_RUNTIME_DIR/hosts.conf"; then
    echo check:generated >> "$DNS_EVENTS"
else
    echo check:initial >> "$DNS_EVENTS"
fi
''')
        self.write_command("unbound-control", '''#!/bin/sh
[ "$1" = -c ] && [ "$2" = "$DNS_CONFIG_FILE" ] && [ "$3" = reload_keep_cache ] || exit 1
grep -q 'updated.danhughes.dev' "$DNS_RUNTIME_DIR/hosts.conf" || exit 1
echo reload:generated >> "$DNS_EVENTS"
if [ ! -f "$DNS_EVENTS.failed" ]; then
    touch "$DNS_EVENTS.failed"
    exit 1
fi
''')
        self.generated.write_text(GENERATED)
        proc = self.start()
        self.until(lambda: self.events.exists() and "started" in self.events.read_text())
        self.generated.write_text(UPDATED)
        self.until(lambda: (self.events.parent / "events.failed").exists())
        self.assertIsNone(proc.poll())
        self.assertEqual((self.runtime / "hosts.conf").read_text(), GENERATED)
        self.until(lambda: self.events.read_text().splitlines().count("reload:generated") == 2)
        self.assertEqual((self.runtime / "hosts.conf").read_text(), UPDATED)
        self.assertEqual(self.events.read_text().splitlines(),
                         ["check:initial", "started", "check:generated", "reload:generated",
                           "check:generated", "reload:generated"])

    def test_invalid_candidate_fails_full_config_check_before_any_reload(self):
        self.write_command("unbound-checkconf", '''#!/bin/sh
[ "$1" = "$DNS_CONFIG_FILE" ] || exit 1
if grep -q INVALID "$DNS_RUNTIME_DIR/hosts.conf"; then
    echo check:invalid >> "$DNS_EVENTS"
    exit 1
fi
echo check:valid >> "$DNS_EVENTS"
''')
        self.write_command("unbound-control", '''#!/bin/sh
echo reload >> "$DNS_EVENTS"
''')
        self.generated.write_text(GENERATED)
        proc = self.start()
        self.until(lambda: self.events.exists() and "started" in self.events.read_text())
        self.generated.write_text(HOME + "INVALID\n")
        self.until(lambda: "check:invalid" in self.events.read_text())
        self.assertIsNone(proc.poll())
        self.assertEqual((self.runtime / "hosts.conf").read_text(), GENERATED)
        self.assertEqual(self.events.read_text().splitlines(), ["check:valid", "started", "check:invalid"])

    def test_exit_when_unbound_exits(self):
        self.generated.write_text(GENERATED)
        self.server.write_text('#!/bin/sh\nexit 0\n')
        proc = self.start()
        self.until(lambda: proc.poll() is not None)
        self.assertEqual(proc.returncode, 0)

    def test_term_reaches_unbound(self):
        self.generated.write_text(GENERATED)
        self.server.write_text('''#!/bin/sh
echo started >> "$DNS_EVENTS"
trap 'echo term >> "$DNS_EVENTS"; exit 0' TERM
while :; do sleep 1 & wait $!; done
''')
        proc = self.start()
        self.until(lambda: self.events.exists() and "started" in self.events.read_text())
        proc.terminate()
        proc.wait(timeout=4)
        self.assertIn("term", self.events.read_text().splitlines())


if __name__ == "__main__":
    unittest.main()
