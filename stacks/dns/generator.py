import ipaddress
import json
import logging
import os
import re
import tempfile
import time
import urllib.request
from pathlib import Path


HOST_CALL = re.compile(r"\bHost\s*\(([^)]*)\)", re.IGNORECASE)
DNS_NAME = re.compile(
    r"(?=.{1,253}$)[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$"
)
LOCAL_ZONE = re.compile(r'^local-zone: "([a-z0-9.-]+)\." static$', re.MULTILINE)
LOGGER = logging.getLogger(__name__)


def hosts_from_routers(routers: list[dict]) -> set[str]:
    hosts = set()
    for router in routers:
        if router.get("status") not in ("enabled", "warning"):
            continue
        rule = router.get("rule")
        if not isinstance(rule, str):
            continue
        matches = list(HOST_CALL.finditer(rule))
        if re.search(r"\bHostRegexp\s*\(", rule, re.IGNORECASE):
            LOGGER.warning("ignoring unsupported HostRegexp in router rule")
        if not matches:
            continue
        for match in matches:
            for host in re.findall(r"`([^`]*)`", match.group(1)):
                host = host.lower().rstrip(".")
                if DNS_NAME.fullmatch(host) and (host == "danhughes.dev" or host.endswith(".danhughes.dev")):
                    hosts.add(host)
    return hosts


def render_hosts(hosts: set[str], ip: str) -> str:
    ipaddress.IPv4Address(ip)
    return "".join(
        f'local-zone: "{host}." static\nlocal-data: "{host}. 60 IN A {ip}"\n'
        for host in sorted(hosts)
    )


def publish(path: Path, content: str) -> bool:
    try:
        if path.exists() and path.read_text() == content:
            if path.stat().st_mode & 0o777 != 0o644:
                os.chmod(path, 0o644)
                return True
            return False

        temporary_path = None
        try:
            with tempfile.NamedTemporaryFile(
                mode="w", dir=path.parent, delete=False, encoding="utf-8"
            ) as temporary:
                temporary_path = Path(temporary.name)
                temporary.write(content)
                temporary.flush()
                os.fsync(temporary.fileno())
            os.chmod(temporary_path, 0o644)
            os.replace(temporary_path, path)
            return True
        except OSError as error:
            if temporary_path is not None:
                temporary_path.unlink(missing_ok=True)
            LOGGER.error("did not publish DNS records: %s", error)
            return False
    except OSError as error:
        LOGGER.error("did not read DNS records: %s", error)
        return False


def fetch_routers(api_url: str) -> list[dict]:
    with urllib.request.urlopen(api_url, timeout=5) as response:
        routers = json.load(response)
    if not isinstance(routers, list) or not all(isinstance(row, dict) for row in routers):
        raise ValueError("Traefik router response is not an array of objects")
    return routers


def poll(
    api_url: str,
    records_path: Path,
    ip: str,
    interval: int = 60,
    iterations: int | None = None,
) -> None:
    count = 0
    while iterations is None or count < iterations:
        try:
            hosts = hosts_from_routers(fetch_routers(api_url))
            if "home.danhughes.dev" not in hosts:
                raise ValueError("Traefik router response does not contain home.danhughes.dev")
            if records_path.exists():
                previous = set(LOCAL_ZONE.findall(records_path.read_text()))
                if len(previous) >= 5 and len(previous - hosts) > len(previous) // 4:
                    raise ValueError(
                        f"implausible DNS contraction: {len(previous)} to {len(hosts)} hosts"
                    )
            if publish(records_path, render_hosts(hosts, ip)):
                LOGGER.info("published %d local DNS records", len(hosts))
        except (OSError, ValueError, json.JSONDecodeError) as error:
            LOGGER.error("did not publish Traefik DNS records: %s", error)
        count += 1
        time.sleep(interval)


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    api_url = os.environ["TRAEFIK_API_URL"]
    records_path = Path(os.environ["RECORDS_PATH"])
    poll(api_url, records_path, os.environ["TRAEFIK_IP"])


if __name__ == "__main__":
    main()
