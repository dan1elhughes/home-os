# doco-cd registry authentication

The external Swarm secret `doco_cd_registry_auth` contains a Docker config
file with Docker Hub credentials. It is mounted at
`/root/.docker/config.json` with mode `0400`. Doco-cd reads this file and
passes the matching registry credentials to Swarm when it deploys services.
The credentials are not service environment variables and are not committed.

The backup is the concealed `DOCKER_AUTH_CONFIG` field in the `home-os deploy`
item in the personal 1Password account's `Swarm Doco-CD` vault. To create the
secret on a new cluster:

```sh
op read --account my.1password.com \
  'op://Swarm Doco-CD/home-os deploy/DOCKER_AUTH_CONFIG' \
  | docker --context swarm secret create doco_cd_registry_auth -
```

For rotation, save the replacement Docker auth file in 1Password, create a
new Swarm secret with a new name, and update this stack's secret reference.
Do not remove the old secret while a service uses it. Deploy the affected
Docker Hub stacks again to refresh the credentials held by Swarm.

The mirror image references do not use Docker Hub authentication. Other
Docker Hub services receive credentials on their next deployment; this change
does not force a restart of all stacks.
