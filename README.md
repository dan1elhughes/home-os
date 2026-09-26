# Home OS

Docker Swarm and infrastructure for the home cluster. The nodes are moving to
Flatcar Container Linux, provisioned by Ignition; persistent data lives on the
TrueNAS box.

## Deploy

Stacks live under `stacks/<name>/` and deploy from inside `stacks/`:

```sh
DOCKER_CONTEXT=swarm ./deploy.sh <stack>
```

See `AGENTS.md` for operating notes and the storage layout.

## Nodes

User, SSH, the TrueNAS mount and swarm membership are provisioned by
[`ignition/`](ignition/README.md). Ansible, `playbook.yml`, `inventory.ini`,
`roles/` and `init.sh` have been retired.

## Databases

The Postgres/MariaDB servers run as a TrueNAS custom app, not on the swarm.
See [`truenas-databases/`](truenas-databases/README.md).
