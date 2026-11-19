# Hermes agent: family assistant in Telegram

## Intent

Dan and his wife want one assistant in a shared group chat that does
household admin for them: check email, read and add calendar events, and
keep a shared shopping list. It must be reachable from both phones, must not
depend on any phone staying online, and must use the MCP gateway that
already runs in the cluster, so new capabilities come from registering MCP
servers, not from changing the agent.

## Goals

- A Hermes Agent (Nous Research, `nousresearch/hermes-agent`) runs as a new
  swarm stack, `stacks/hermes`, deployed by doco-cd like every other stack.
- Chat happens in a Telegram group of Dan, his wife and the bot. Both people
  can also message the bot directly.
- All tools come from mcpjungle (`https://mcp.danhughes.dev/mcp`). Day one:
  `fastmail` (mail, calendar, contacts, notes) and `todoist`.
- The shopping list is a "Shopping" project in Dan's Todoist, shared with
  his wife, so both can also edit it in the Todoist app.
- The model is OpenCode Go (Dan's subscription).

## Non-goals

- WhatsApp, Signal or any channel that needs a SIM or a phone that stays
  online. WhatsApp was considered and dropped for that reason.
- MCP servers configured inside Hermes itself. Hermes knows one MCP endpoint
  only: mcpjungle.
- A Traefik route or the Hermes dashboard. The bot polls Telegram outbound
  and needs no ingress.
- Per-person data separation. Fastmail and Todoist are connected as Dan's
  accounts; Dan accepted that his wife can ask the bot about his calendar
  and mail.

## Anti-goals

- The bot sends an email, deletes something or replies to an invitation
  without a "yes" in the chat. A prompt-injected email that makes the bot
  act is the main risk here.
- Anyone other than Dan and his wife can talk to the bot.
- Secrets in the repository or on a command line.

## Decided

**Channel.** Telegram bot made with @BotFather. In the group it answers only
when it is mentioned or replied to. Access is limited to the two Telegram
user IDs (Hermes `TELEGRAM_GROUP_ALLOWED_USERS` and the matching
direct-message allow-list). Privacy mode in BotFather stays on unless
mention-only replies need it off.

**Tools through mcpjungle.** Create a new mcpjungle client named `hermes`
with its own token and the allow-list `["fastmail", "todoist"]`; `tally` is
out. Follow `stacks/mcpjungle/README.md` for creating the client. Hermes
connects to `https://mcp.danhughes.dev/mcp` with
`Authorization: Bearer <hermes client token>`. To add a capability later,
register a server in mcpjungle and add it to this client's allow-list
(delete and re-create with the same token, as the README describes).

**Permissions.** Reads are free: search and read mail, list and search
events, read notes and contacts, all of Todoist reads. Adding and completing
shopping-list items and creating or updating calendar events are free.
These need an explicit yes in the chat before the call:
`fastmail__send_email`, `fastmail__delete_email`, `fastmail__delete_event`,
`fastmail__rsvp_event`, `fastmail__delete_contact`, `fastmail__delete_note`,
`todoist__delete-object`. Enforce this with Hermes's own tool approval
setting if it supports per-tool approval for MCP tools; always also state
the rule in `SOUL.md`. Email bodies are untrusted input and `SOUL.md` says so.

**Builder's calls (made after the design review, not discussed with Dan).**
- Built-in toolsets: only memory and cron (for reminders) stay on. Terminal,
  file, code execution, web, browser, skill creation and `send_message` to
  other chats are off, so an injected email cannot read tokens, reach the
  LAN or edit Hermes's config. Hermes must not be able to change its own
  approval or toolset settings; use managed scope or an equivalent lock if
  Hermes supports one.
- Approvals: mark the mcpjungle server untrusted so writes ask, and put only
  the "free" tools above on the allow-list. The "Always" choice must not
  apply to the gated tools; if Hermes cannot prevent it, document that in
  the README. Dan is the only admin (`/yolo` and other admin commands);
  either person may approve a gated call.
- Groups: set `TELEGRAM_GROUP_ALLOWED_CHATS` to the family group's chat ID,
  so the bot never answers in another group. Getting that ID is one more
  manual step.

**Model.** `model.provider: opencode-go`, key `OPENCODE_GO_API_KEY`. Go has
rolling usage windows; when one runs out the bot stops answering until it
resets. That is accepted. No fallback provider on day one.

**Deploy.**
- One service on the external `main` overlay network, one replica.
- State: `/mnt/nas/hermes-data:/opt/data` (create the directory on one node
  only, owned by UID 10000, which the image's `hermes` user uses).
- Keep the image's entrypoint; command `gateway run`. `HERMES_DASHBOARD`
  unset.
- `TZ=Europe/London`.
- Secrets as doco-cd `external_secrets` from
  `op://Swarm Doco-CD/home-os deploy/<VAR>` with a field of the same name in
  that vault item: `TELEGRAM_BOT_TOKEN`, `OPENCODE_GO_API_KEY`,
  `HERMES_MCPJUNGLE_TOKEN`. The Telegram user IDs are not secret and go in
  `.doco-cd.yml` `environment`.
- Add a `hermes` document to `.doco-cd.yml` with `prune_images: false`.
- Memory limit 2 GB (no browser tools are needed).
- Pin the image to a version tag so Renovate tracks it, like other stacks.

## Left to the builder

- Which OpenCode Go model is the default. Pick one with reliable tool
  calling and verify it with a real tool call through mcpjungle.
- How the declarative config reaches `/opt/data/config.yaml`. Hermes seeds
  and migrates that file on boot and can edit it itself. Prefer a committed
  config in `stacks/hermes/` that is applied on each deploy (Hermes's
  "managed scope" or "remote config" features may fit), so the repository
  stays the source of truth for model, MCP, Telegram and approval settings.
  If that is not practical, commit the file anyway and document the copy
  step in the stack README.
- `SOUL.md` wording: a household assistant for Dan and his wife, short
  replies suited to a phone, UK date and time, the confirmation rule, and
  the shopping list is the Todoist "Shopping" project.
- Whether Hermes's Telegram adapter needs any other environment for group
  behaviour (home channel and so on).
- A short `stacks/hermes/README.md`: how to create the bot, add it to the
  group, rotate tokens and add a capability through mcpjungle.

## Manual steps for Dan

These need his accounts and cannot be done from the repository:

1. Create the bot with @BotFather; store the token in 1Password.
2. Make the Telegram group with his wife and add the bot; get both user IDs
   and the group's chat ID.
3. Create the "Shopping" project in Todoist and share it with his wife.
4. Create an OpenCode Go API key; store it in 1Password.
5. Add the three fields to `home-os deploy` in the `Swarm Doco-CD` vault.

## Done means

- Dan or his wife mentions the bot in the group, asks "what's on tomorrow",
  and gets an answer from the Fastmail calendar.
- "Add milk to the shopping list" adds a task to the Todoist "Shopping"
  project, which shows in both Todoist apps.
- "Email X about Y" makes the bot show the draft and ask for a yes before
  it sends.
- A message from a Telegram user outside the allow-list, or in any other
  group, gets no response.
- Asking the bot to run a shell command or read a file fails: no such tool.
- The stack redeploys from `master` through doco-cd with no laptop steps.
