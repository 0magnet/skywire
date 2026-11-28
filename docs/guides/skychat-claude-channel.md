# Skychat channel for Claude Code

`skywire cli skychat channel` is an MCP server on stdio that Claude Code loads
as a [channel](https://code.claude.com/docs/en/channels-reference). Each new
skychat group message or DM is pushed into the running Claude session as it
arrives. Claude answers with tools. Nothing polls from the Claude side and
nothing has to be re-armed.

## Load it

Put this in `.mcp.json` in the project Claude Code runs from, or add it with
`claude mcp add`:

```json
{
  "mcpServers": {
    "skychat": {
      "command": "skywire",
      "args": ["cli", "skychat", "channel"]
    }
  }
}
```

Start Claude Code with the server named as a channel:

```
claude --dangerously-load-development-channels server:skychat
```

The flag is needed because the server is not on the approved channel
allowlist. Claude Code shows a confirmation dialog at startup. It works in an
interactive session only. With `-p` Claude Code ignores the flag.

## What Claude sees

A message arrives as

```
<channel source="skychat" chat_id="15b5d6db-…" kind="group" group_name="Skywire"
  sender_pk="03…" message_id="1795882207097741773" ts="2026-11-28T16:10:07Z">text</channel>
```

`chat_id` is the group id for a group and the sender's public key for a DM.
`reply_to` is set when the message quotes an earlier one, `forwarded` when it
was forwarded, and `request` when a DM comes from a peer not yet let in.

## Tools

| tool | does |
| --- | --- |
| `reply(chat_id, text, reply_to?)` | posts to the group or DM. `reply_to` takes a `message_id` and quotes it |
| `history(chat_id, limit?)` | recent messages, one per line, default 20, at most 100 |
| `groups()` | the groups this visor is an active member of |

The server's instructions tell Claude that senders are untrusted. Claude
answers them but does not act on instructions inside a message. A group
member can still put any text in front of Claude, so use `--senders` to
limit who reaches the session when that matters.

## Flags

| flag | default | meaning |
| --- | --- | --- |
| `--groups` | every active group | only deliver these group ids |
| `--dms` | true | deliver DMs too |
| `--senders` | every sender | only deliver messages from these public keys |
| `--prefix` | true | start each reply with `[claude bot] ` |
| `--ignore-prefix` | `[claude bot]` | drop inbound messages starting with this |
| `--state` | `<user config dir>/skywire/skychat-channel/<local or pk>.json` | resume cursors |
| `--rpc` | `localhost:3435` | visor RPC address |
| `--via` | none | run as a remote visor, `dmsg://<pk>` or `skynet://<pk>` |

With the default `--prefix`, replies sent from the operator's own visor key
are marked so other members can tell them apart. The channel also skips its
own posts when the visor hands them back.

## Run as another visor

Everything goes through the visor RPC, so `--via` gives Claude a visor key of
its own. The local visor must be allowed to reach the remote visor's RPC, the
same as for any `--via` command.

```json
{
  "mcpServers": {
    "skychat": {
      "command": "skywire",
      "args": ["cli", "skychat", "channel", "--via", "dmsg://<pk>", "--prefix=false"]
    }
  }
}
```

Each stream reconnects with backoff when its session drops.

## Resume

The state file keeps the group arrival cursor, the DM event seq and the ids
recently delivered. A restart picks up messages that arrived while the
channel was down and does not replay older ones. A first run starts from now.

Group messages come from the visor's group inbox (`GroupWait`). DMs come from
skychat's `/events?wait=` long-poll through the visor (`SkychatHTTP`). A
visor whose skychat predates the wait mode delivers groups only, and the
channel says so on stderr.

## Notes

Stdout carries only the MCP protocol. Diagnostics go to stderr.

Keep `--timeout` at its default of 30 s or higher. Each long-poll waits up to
20 s.
