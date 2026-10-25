#!/usr/bin/env bash
# Checks the deploy configuration that doco-cd applies. Run locally before a
# push, and in CI before a merge. Every compose file is interpolated with
# dummy values for the deploy-time variables, because the real ones come from
# 1Password at deploy time and are never present here.
set -euo pipefail
cd "$(dirname "$0")/../.."

for var in $(grep -hoE '\$\{[A-Za-z_][A-Za-z0-9_]*' \
    stacks/*/docker-compose.yml truenas-databases/docker-compose.yml \
    | sed 's/\${//' | sort -u); do
  export "$var=dummy"
done

for file in stacks/*/docker-compose.yml truenas-databases/docker-compose.yml; do
  docker compose -f "$file" config -q
  echo "ok   $file"
done

python3 - <<'PYTHON'
import sys
from pathlib import Path

import yaml

root = Path(".")
deployed = set()
errors = []

for doc in yaml.safe_load_all((root / ".doco-cd.yml").read_text()):
    if not doc:
        continue
    name = doc.get("name", "<unnamed>")
    working_dir = doc.get("working_dir")
    if not working_dir:
        errors.append(f"{name}: no working_dir")
        continue
    path = root / working_dir
    if not path.is_dir():
        errors.append(f"{name}: {working_dir} does not exist")
    elif not (path / "docker-compose.yml").is_file():
        errors.append(f"{name}: {working_dir} has no docker-compose.yml")
    deployed.add(path.resolve())

for stack in sorted((root / "stacks").iterdir()):
    if stack.is_dir() and (stack / "docker-compose.yml").is_file():
        if stack.resolve() not in deployed:
            errors.append(f"{stack.name}: not listed in .doco-cd.yml")

for error in errors:
    print("FAIL", error)
print("ok   .doco-cd.yml" if not errors else f"{len(errors)} problem(s) in .doco-cd.yml")
sys.exit(1 if errors else 0)
PYTHON
