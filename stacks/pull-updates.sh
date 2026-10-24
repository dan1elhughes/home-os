#!/usr/bin/env bash
set -euo pipefail

PRS=$(gh pr list --state open --json number -q '.[].number')

if [ -z "$PRS" ]; then
    echo "No open PRs found."
    exit 0
fi

STACKS=()

for PR in $PRS; do
    while IFS= read -r stack; do
        [ -n "$stack" ] && STACKS+=("$stack")
    done < <(gh pr view "$PR" --json files -q '.files[].path' | awk -F/ '/^stacks\//{print $2}' | sort -u)

    gh pr merge "$PR" --squash --delete-branch
done

git pull --ff-only

# doco-cd picks up merged changes on its next poll; report what it will apply.
if [ ${#STACKS[@]} -eq 0 ]; then
    echo "No stack changes found."
    exit 0
fi

printf 'Wait for the next poll: doco-cd will apply %s\n' \
    "$(printf '%s\n' "${STACKS[@]}" | sort -u | paste -sd' ' -)"
