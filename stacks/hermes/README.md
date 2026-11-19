# hermes

[Hermes Agent](https://hermes-agent.nousresearch.com/docs/) as a family
assistant in a Telegram group of Dan, his wife and the bot. Design:
`docs/superpowers/specs/2026-11-19-hermes-agent-design.md`.

The bot polls Telegram outbound. The web dashboard is at
`https://hermes.danhughes.dev` (user `dan`, password in 1Password as
`HERMES_DASHBOARD_BASIC_AUTH_PASSWORD`). Hermes refuses to serve it on a
non-loopback bind without a sign-in provider, so it keeps its own password
even on the internal domain. Settings in the managed config cannot be
changed from the dashboard. Its tools all come from mcpjungle through the client `hermes`,
whose allow-list is `fastmail`, `todoist` and `exa`. Its model is OpenCode
Go (`kimi-k3`).

## Files

| Repository | Container | Purpose |
| --- | --- | --- |
| `managed/config.yaml` | `/etc/hermes/config.yaml` | Managed scope: model, MCP, toolsets, approvals, Telegram gates |
| `SOUL.md` | `/opt/data/SOUL.md` | Identity and house rules |
| `/mnt/nas/hermes-data` | `/opt/data` | Sessions, memory, cron jobs, the user `config.yaml` |

Both repository files are swarm configs, so a change rolls the service.
Each leaf in the managed file wins over `/opt/data/config.yaml`, and
`hermes config set` refuses to change it. That keeps these settings out of
the bot's and the chat's reach:

- Only the `memory`, `cronjob` and `mcp-mcpjungle` toolsets. There is no
  terminal, file, code or browser tool, so an email that tries to give the
  bot instructions has nothing local to use.
- `trust: untrusted` on mcpjungle, so every tool call asks for approval,
  except the tools in `command_allowlist`: reads, Exa search, Todoist task
  changes and email drafts. Calendar event writes ask, because an event with
  attendees sends invitation email. Exa fetch asks, because a URL can carry
  mail contents out.
- Sending email, deleting anything and RSVPs always ask. An "Always" answer
  is held in memory until the next restart, because the managed list
  replaces the list that Hermes writes to `/opt/data/config.yaml`. To clear
  it at once, run `docker service update --force hermes_hermes`.
- The bot only answers in the family group (`allowed_chats`) and in direct
  messages from the two users (`TELEGRAM_ALLOWED_USERS`), and in the group
  only when someone mentions it or replies to it.
- Dan is the only admin (`/yolo`, `/model`, and so on). Both can approve.

## Secrets and IDs

| Name | Where |
| --- | --- |
| `TELEGRAM_BOT_TOKEN` | 1Password `Swarm Doco-CD` / `home-os deploy` |
| `OPENCODE_GO_API_KEY` | 1Password `Swarm Doco-CD` / `home-os deploy` |
| `HERMES_MCPJUNGLE_TOKEN` | 1Password `Swarm Doco-CD` / `home-os deploy` (the mcpjungle client token) |
| `HERMES_DASHBOARD_BASIC_AUTH_PASSWORD`, `HERMES_DASHBOARD_BASIC_AUTH_SECRET` | 1Password `Swarm Doco-CD` / `home-os deploy` |
| `TELEGRAM_ALLOWED_USERS`, `TELEGRAM_FAMILY_CHAT`, `TELEGRAM_ADMIN_USER` | `.doco-cd.yml`, not secret |

The gateway logs only hashed IDs and consumes every update, so to read a new
Telegram ID, stop it, have the person message the bot, and read the queue.
The `-K` config keeps the token off the command line:

```bash
docker --context swarm service scale hermes_hermes=0
curl -s -K <(printf 'url = "https://api.telegram.org/bot%s/getUpdates"\n' \
    "$(op read 'op://Swarm Doco-CD/home-os deploy/TELEGRAM_BOT_TOKEN')") \
  | jq '.result[].message | {chat: .chat.id, from: .from.id, name: .from.first_name}'
docker --context swarm service scale hermes_hermes=1
```

To rotate a token, change the 1Password field and redeploy
(`cd stacks && ./deploy-local.sh hermes`, or commit any change to this
stack). For the mcpjungle token, re-create the `hermes` client with the new
token as `stacks/mcpjungle/README.md` describes.

## Add a capability

1. Register the MCP server in mcpjungle (`stacks/mcpjungle/README.md`).
2. Add it to the `hermes` client's allow-list (delete and re-create with
   the same token, as that README describes).
3. Add its read-only tools to `command_allowlist` in `managed/config.yaml`
   as `mcp:mcpjungle/<server>__<tool>`. Anything not listed asks first.
4. Commit. The service rolls with the new config.

## Voice, photos and reactions

- Voice notes are transcribed in the container by faster-whisper (`small`,
  English); the transcript is not echoed to the chat. OpenCode has no
  transcription endpoint, and its relay rejects audio input. The package
  lives in `/opt/data/installs` and the model in `/opt/data/.cache`, so both
  survive restarts. On a fresh data directory, enable the extra once, then
  restart the service (the install takes several minutes on NFS):
  `docker exec -u hermes -w /opt/hermes -e HERMES_HOME=/opt/data -e PYTHONPATH=/opt/hermes <ctr> /opt/hermes/.venv/bin/python -c 'import pm; pm.ensure_import("stt-whisper")'`.
  It ends with "restart Hermes to activate it", which is expected.
- Spoken replies use Edge TTS (`en-GB-SoniaNeural`, no key). They are off
  until someone sends `/voice on` (reply in voice to voice notes) or
  `/voice tts` (always) in a chat.
- Photos go to `kimi-k3` natively; the auxiliary describer is the same model.
- The bot reacts 👀 while it works, then 👍 or 👎.

## Model

Change `model.default` in `managed/config.yaml`. OpenCode Go has rolling
usage windows; when one is used up, the bot stops answering until it
resets. `docker exec <ctr> hermes usage` shows the state.
