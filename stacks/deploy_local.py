#!/usr/bin/env python3
"""Deploy one stack from the laptop's working tree. deploy-local.sh injects
the 1Password environment. Config and secret objects are named the way
doco-cd names them (<stack>_<basename>_<sha8) so a local iteration and the
later commit do not double-roll."""

import hashlib
import re
import subprocess
import sys
from pathlib import Path

STACKS = Path(__file__).resolve().parent
CONVERTED = ".deploy-local-compose.yml"


def sha8(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()[:8]


def materialise(kind: str, name: str, data: bytes) -> None:
    listed = subprocess.run(
        ["docker", "--context", "swarm", kind, "ls", "--format", "{{.Name}}"],
        capture_output=True, text=True, check=True).stdout
    if name in listed.splitlines():
        return
    subprocess.run(
        ["docker", "--context", "swarm", kind, "create", name, "-"],
        input=data, capture_output=True, check=True)


def main() -> None:
    stack = sys.argv[1]
    sdir = STACKS / stack
    text = (sdir / "docker-compose.yml").read_text()
    out = text
    for kind in ("configs", "secrets"):
        section = re.search(rf"(?ms)^{kind}:\n(?!--).*(?=\Z)", out)
        if not section:
            continue
        body = section.group(0)

        def body_repl(m: re.Match) -> str:
            key, file = m.group(1), m.group(2)
            path = (sdir / file.removeprefix("./")).resolve()
            data = path.read_bytes()
            obj = f"{stack}_{path.name}_{sha8(data)}"
            materialise(kind.rstrip("s"), obj, data)
            return f"  {key}:\n    external: true\n    name: {obj}\n"

        out = out.replace(body, re.sub(
            r"^  ([A-Za-z0-9_-]+):\n    file: (\S+)\n", body_repl, body,
            flags=re.M))
    target = sdir / CONVERTED
    target.write_text(out)
    try:
        subprocess.run(
            ["docker", "--context", "swarm", "stack", "deploy",
             "--detach=false", "-c", str(target), stack],
            check=True)
    finally:
        target.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
