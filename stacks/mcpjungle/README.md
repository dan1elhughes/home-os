# mcpjungle

MCP gateway for AI clients. Deployed as a swarm service behind Traefik at
`mcp.danhughes.dev`, running in enterprise mode: every request needs a
token, and every client identity is allow-listed per server.

This README documents how to register a new MCP server and how to let
existing clients call it. The second part is the one that costs time to
work out.

## Where things live

- Service: `mcpjungle_mcpjungle`, one replica. It moves between nodes on
  redeploy — find it with
  `docker service ps mcpjungle_mcpjungle --filter desired-state=running`.
- CLI: `/mcpjungle` inside the container. Its admin config is mounted at
  `/root/.mcpjungle.conf` (backed by `/mnt/nas/mcpjungle-conf/` on the
  host), so `docker exec <ctr> /mcpjungle ...` works with no extra flags.
- State: servers, tools, clients and users all live in the Postgres
  database on the NAS (see `truenas-databases/`). Registrations survive
  container recreates. Nothing about them is stored in this repo.

The examples below assume `ssh <node>` and this shorthand:

```bash
C=$(docker ps --filter name=mcpjungle -q)
```

## Register a new server (streamable HTTP)

If the server needs no auth, one line is enough:

```bash
docker exec $C /mcpjungle register --name <name> --url https://example.com/mcp
```

Names are unique across the gateway and may not contain spaces, special
characters or consecutive underscores.

For a server that needs a bearer token or custom headers, use a JSON
config file with `register -c`. Build it locally, then pipe it over ssh
stdin so the token never appears in a command line, in `ps` output or in
shell history:

```bash
# new-server.json locally:
# {
#   "name": "example",
#   "transport": "streamable_http",
#   "description": "What the server provides",
#   "url": "https://example.com/mcp",
#   "bearer_token": "<token>",
#   "headers": { "X-Custom": "value" }
# }
ssh <node> 'docker exec -i $(docker ps --filter name=mcpjungle -q) sh -c "
  cat > /root/new-server.json && /mcpjungle register -c /root/new-server.json
  rc=\$?; rm -f /root/new-server.json; exit \$rc
"' < new-server.json
rm new-server.json
```

Config files also support `${VAR}` placeholder substitution from the
environment where the CLI runs.

A successful registration prints every tool, prompt and resource it
discovered. Check the state later with `list servers` and `list tools`;
remove a server with `deregister <name>`.

Prefer servers that accept a static bearer token. Servers that only speak
OAuth need a browser flow during registration (the `oauth_*` config
fields) — see
[the upstream docs](https://docs.mcpjungle.com/guides/register-http-servers)
for that path.

## Allow existing clients to use the new server

Registration alone is not enough. In enterprise mode, every MCP client
identity has an explicit allow-list of servers, and a new server is
invisible to all existing clients until you add it to their list. The
gateway rejects calls with an error that does not mention allow-lists,
which makes this easy to miss.

Check who needs the change:

```bash
docker exec $C /mcpjungle list mcp-clients
```

In 0.4.6, neither the CLI nor the API can edit a client's allow-list —
`update mcp-client` only rotates the access token. The supported way is
to delete the client and create it again, supplying the same access
token it had before. `create mcp-client` accepts a custom token, so the
token string stays byte-identical and client configs need no changes.

Do the delete and re-create back to back so the auth gap stays
negligible, and pipe the request body over stdin:

```bash
# client-body.json locally:
# {
#   "name": "<client>",
#   "description": "<keep the old description>",
#   "access_token": "<the client's existing token, unchanged>",
#   "allow_list": ["existing-server-1", "existing-server-2", "<new-server>"]
# }
ssh <node> 'docker exec -i $(docker ps --filter name=mcpjungle -q) sh -c "
  ADMIN=\$(sed -n \"s/^access_token:[[:space:]]*//p\" /root/.mcpjungle.conf)
  curl -s -X DELETE -H \"Authorization: Bearer \$ADMIN\" \
    http://localhost:8080/api/v0/clients/<client>
  curl -s -X POST -H \"Authorization: Bearer \$ADMIN\" \
    -H \"Content-Type: application/json\" --data-binary @- \
    http://localhost:8080/api/v0/clients
"' < client-body.json
rm client-body.json
```

`204` followed by `201` means it worked. The client token comes from
wherever the client's config lives (for opencode: the `mcp` block in
`~/.config/opencode/opencode.json`). Take it from there and re-supply it
exactly — do not let the re-create generate a new one. Clients read
their row from the database on every request, so nothing needs a
restart.

## Verify end to end

A token that initializes cleanly proves the client row survived the
re-create; a tool call proves the allow-list and the upstream auth too.
The streamable HTTP handshake on `https://mcp.danhughes.dev/mcp` needs
three steps with the client token in the `Authorization: Bearer` header:

1. `initialize` — the response carries an `Mcp-Session-Id` header. Save
   it; later calls fail without it.
2. `notifications/initialized` — returns an empty body. That is the ACK,
   not an error.
3. `tools/call` with the session id, for a cheap read-only tool on the
   new server.

## Caveats

- The allow-list limitation above is pinned to image `0.4.6`. Newer
  releases may add allow-list editing — check `update mcp-client
  --help` before reaching for delete + re-create.
- `deregister` drops a server's tools at once; registering again
  restores them. Client tokens and allow-lists are separate from server
  registrations and are not touched by it.
