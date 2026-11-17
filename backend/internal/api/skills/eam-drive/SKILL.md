---
name: eam-drive
description: Read, write, list, search and watch files on the user's own Everything About Me drive (everythingabout.me) over its HTTP API, behind a scoped bearer token. Trigger when the user says 我的网盘 / 我的盘 / 个人网盘 / 存到我盘里 / 从盘里拿 / everythingabout.me / EAM, pastes an eam_ token or an https://everythingabout.me/s/ link, or asks to save, upload, download, find or keep an eye on files on their personal drive instead of the local disk.
---

# Everything About Me drive

The user's own file drive: a folder tree with stable ids and a change feed, reached over HTTP with a
bearer token. It is the user's data, not scratch space — write where they asked, and do not reorganize
a tree you were not asked to organize.

This file is the part that does not change per connection. The part that does — grants, folder scope,
expiry, and the write path for this connection's transfer class — is generated from the live token row
on every request. Read that before the first call.

## 1. Find the connection

A connection is two facts: the API origin and a token. Usually the token is a `/s/` link, which
carries both. From a link:

    API="${LINK%/s/*}"; TOKEN="${LINK##*/s/}"

A bare token does not name a host: ask for the origin rather than guessing one, because a self-hosted
drive is a different deployment.

### Look for one first

Look in these places, in this order, so a drive that is already set up is never set up twice:

1. `$EAM_DRIVE` — a link like `https://<host>/s/<token>`, set for this task on purpose.
2. `"${XDG_CONFIG_HOME:-$HOME/.config}/eam/drive"` — the saved connection, one line, mode 0600.
3. What the user just pasted.

### First run: nothing saved yet

A link the user pasted is the connection — use it. Only if there is none, walk them through making one.
They cannot be handed a token; they have to open the page, because management is session-only on the web
and a drive token deliberately cannot mint or widen another:

- Open `<host>/connect`, press `Add app` (`添加 App` if their interface is Chinese), and copy the
  one-line prompt it shows. The host is `everythingabout.me` unless they run their own instance — ask
  rather than assume.
- Paste that line here.

Either way, once the link works:

- Name the connection so the owner's page says what it is rather than `Unnamed app`: `PATCH {api}/me`
  with `{"name": "..."}` and the Bearer token. This is the only write a credential has about itself.
- Save it as the default, and say where:

      mkdir -p -m 700 "${XDG_CONFIG_HOME:-$HOME/.config}/eam"
      (umask 077 && echo "$LINK" > "${XDG_CONFIG_HOME:-$HOME/.config}/eam/drive")

### Every run after: no paste, no questions

Read the saved file, fetch `/s/<token>` (§2), and get to work. When the connection is there and valid,
do not narrate the setup — the user asked for their files, not for a status report.

- `404` from `/s/`: the saved connection was revoked or expired. Ask for a new prompt, replace the file
  when it arrives, and do not delete it on your own.
- Once a default is saved, a link the user pastes for one task is not that default: use it for the task
  and leave the file alone unless they say otherwise.

## 2. Read the live instructions first

    LINK='https://<host>/s/<token>'
    curl -fsSL "$LINK"

    # given only a bare token
    curl -fsSL "$API/s/$TOKEN"

That page is rendered from the live token row, so it states the grants, scope, expiry and transfer
class the connection has *now* — including a permission the owner removed after the link was minted.
Remembered API details are not a substitute:

- Where it disagrees with anything you remember about this API, the page wins.
- Fetch it once per session, not once per request.
- `/s/` answers `404` once the connection is revoked or expired, and the API answers `401` with that
  token — the same event from two places. Do not retry, and do not go looking for another credential.

## 3. Rules that hold for every connection

- **The token is a credential.** Do not commit it, write it into a project file, echo it into output,
  or attach it to a URL you built for someone else. A `/s/` link is the same secret in a friendlier
  shape.
- **Scope is a boundary, not a bug.** A `403` or `404` outside the connection's folder means stop and
  say so, rather than hunting for a path that gets around it.
- **Deletes are permanent.** There is no trash. Removing a non-empty folder needs an explicit
  recursive flag, and the API never recurses on your behalf.
- **Create is the default; replacing is explicit.** Do not overwrite a file you have not read in this
  task unless the user asked you to replace it. A `409` or `412` is the drive protecting bytes you
  did not see — re-read and decide, never retry blind.
- **Moves and copies are Operations.** `Prefer: respond-async` answers `202` with a `Location`; follow
  that Operation to a terminal state instead of re-issuing the request.
- **`429` carries `Retry-After`.** Wait it out, then retry with jitter.

## 4. Install

This document is served at `/skill` on every Everything About Me host, and that URL stays the current
copy — re-fetch it after an upgrade:

    mkdir -p ~/.claude/skills/eam-drive
    curl -fsSL https://<host>/skill -o ~/.claude/skills/eam-drive/SKILL.md

Agents that read the shared `~/.agents/skills/` root can install the same file under
`~/.agents/skills/eam-drive/SKILL.md`.
