#!/usr/bin/env bash
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
op run --environment q5pfvfpsh44h7xywiqys2ff5ma --account BMVXTKJWJNHKDFO36EZROR44HM -- \
  python3 "$here/deploy_local.py" "$@"
