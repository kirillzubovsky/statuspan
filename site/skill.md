---
name: statuspan
description: "Post job status to a local or self-hosted StatusPan board so a person can see which agent is working on what. Use when starting, updating, blocking, finishing, or failing a finite job, and when a recurring job becomes blocked or failed."
---

# StatusPan

StatusPan is a local airport-style board. You push. It does not call you back. The local daemon has no auth and listens only on loopback (`127.0.0.1:4477`). Another machine posts to a self-hosted relay instead.

The board is optional. A failed, slow, or unreachable StatusPan must never block, delay, or fail the real work. Time out in about 2 seconds and ignore errors (`curl --max-time 2 ... || true`).

Full detail: <https://statuspan.com/llms-full.txt>. Machine-readable: `statuspan agent json`. This file is enough to post correctly.

On a Mac, [StatusPan.dmg](https://github.com/kirillzubovsky/statuspan/releases/latest/download/StatusPan.dmg) is a WKWebView window on the local board. It starts the bundled `statuspan serve` if nothing answers on `STATUSPAN_ADDR` (default `127.0.0.1:4477`), and it stops only a daemon it started. The CLI install, on macOS and Linux, is `curl -fsSL https://statuspan.com/install.sh | sh`, which installs to `~/.local/bin/statuspan`.

## What belongs on the board

Post finite jobs that have a finish line.

Keep healthy recurring jobs and routines off the board: cron, daily digests, watchers, and hourly runs that are going fine. Do not open a card for a quiet run.

Exception: if a recurring job becomes `blocked` or `failed`, push it so it shows under Needs attention. When it is fixed or unblocked, push `done` so it clears. `statuspan run --recurring` posts nothing while that command runs and nothing on exit 0, and posts one `failed` on a non-zero exit. You still push `done` yourself once the recurring job is resolved.

Only use `blocked` when you actually asked a human for input or approval. Do not use it as a synonym for "still working".

A routine may post a different job when that run produces real, finishable work. A quiet run posts nothing.

## Install the skill

```bash
statuspan agent install
```

That writes this file where agents look for skills. `statuspan agent` prints it. `statuspan agent prompt` prints a short snippet for AGENTS.md or CLAUDE.md. `statuspan agent json` prints a discovery document. Never print tokens.

## Three ways to report

### 1. Wrap the command

`statuspan run` posts `running` (progress 0), heartbeats on a timer, then `done` (exit 0, progress 100, note `Completed in <duration>`) or `failed` (exit N, note `Exited with code N after <duration>`). StatusPan exits with the command's exit code. A signal is `128` plus the signal number. The child stays in StatusPan's process group, so an interactive command keeps the terminal. StatusPan catches SIGINT and SIGTERM, forwards SIGTERM to the child, and forwards SIGINT only when stdin is not a terminal. If the command cannot be started, both normal and `--recurring` modes post `failed` with a note like `Could not start: ...` and StatusPan exits 127.

Posts before the command finishes are fire-and-forget. The final post is sent with about a 2 second timeout, then StatusPan exits even if the board is down. A down board is a warning on stderr only with `--verbose`. A bad jobs endpoint or an incomplete relay setup is one warning on stderr, no posts, and the command still runs. The token is not printed.

```bash
statuspan run --agent "My Agent" --job build-1 --title "Build v1" -- make test
statuspan run --recurring --agent "My Agent" --job digest --title "Daily digest" -- python digest.py
```

The default agent is `STATUSPAN_AGENT`. The title defaults to the job id. `--ttl` is the heartbeat window in seconds (default 600). The heartbeat interval defaults to `min(ttl/2, 60s)` and is at least 10 seconds. `--heartbeat SEC` replaces that interval with no floor (tests use `1`). `--link` is optional.

### 2. Post one update

The CLI talks to the local daemon unless a relay is configured.

```bash
statuspan post --agent "My Agent" --job build-1 --state running --title "Build v1" --progress 40 --note "halfway"
```

Relay environment (all three are required):

- `STATUSPAN_RELAY_URL`
- `STATUSPAN_RELAY_WORKSPACE`
- `STATUSPAN_TOKEN` or `STATUSPAN_TOKEN_FILE`

The same values as flags: `--relay`, `--workspace`, and `--token` or `--token-file`. The CLI sends `Authorization: Bearer`. Never put the token in a command you log. Prefer `--token-file` or `STATUSPAN_TOKEN_FILE`.

`post` returns an error and does not post locally when the relay settings are incomplete, the token file cannot be read, `--url` or `--addr` is invalid, or `--url` is combined with a relay. `run` prints one warning and still runs the command, with no posts.

`STATUSPAN_URL` (or `--url`) is the full local jobs endpoint, for example `http://127.0.0.1:14477/api/jobs`, for both `post` and `run`. `STATUSPAN_ADDR` is `host:port` (default `127.0.0.1:4477`) when no URL override is set.

If you omit `--seq`, `post` and `run` send epoch milliseconds. That value is strictly increasing inside the process. Pass `--seq` only when you already chose one.

### 3. Raw HTTP

Use this when the `statuspan` binary is not installed. Body is one JSON object, or an array of objects. `Content-Type: application/json`. The daemon and the relay both accept an array. Unknown fields are ignored.

Local daemon, no auth:

```bash
curl --max-time 2 -s -X POST http://127.0.0.1:4477/api/jobs \
  -H 'Content-Type: application/json' \
  -d '{"agent":"My Agent","job_id":"build-1","title":"Build v1","state":"running","progress":40}' \
  || true
```

Relay:

```bash
curl --max-time 2 -s -X POST "$STATUSPAN_RELAY_URL/v1/w/$STATUSPAN_RELAY_WORKSPACE/jobs" \
  -H "Authorization: Bearer $STATUSPAN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"agent":"My Agent","job_id":"build-1","state":"running","progress":40}' \
  || true
```

Do not print the token. A write token cannot read. A read token cannot post. If you were not given a relay token, post to the local daemon.

List jobs with `GET http://127.0.0.1:4477/api/jobs` or `statuspan ls`.

Any language is fine. Use a 2 second timeout and ignore connection errors. Keep doing the real work.

## Payload

A new job needs `agent`, `job_id`, and `state`. Later posts need `agent` and `job_id`. Omitted fields stay as they are. Send `""` only when you mean to clear `title`, `link`, or `note`.

| Field | Rule |
| --- | --- |
| `agent` | Required. Not blank. No `/`, `\`, or NUL. |
| `job_id` | Required. Same rules. The card is `(agent, job_id)`. Reuse the slug for every update of that work. |
| `state` | `queued`, `running`, `blocked`, `done`, or `failed`. Required on a new job. Omit later to leave it. |
| `title` | Short label. Optional. |
| `progress` | Integer 0 through 100. Optional. |
| `link` | Any string, often an `https://example.com/...` URL. Optional. Not validated as a URL. |
| `note` | One line of status. Optional. |
| `seq` | Optional integer. On raw HTTP, omit it and the post applies (the stored seq is left unchanged). If you set it, only a strictly greater seq changes the job. An equal or older seq is kept in history with `applied: false` and does not move the heartbeat clock. |
| `ttl` | Optional seconds, `>= 0`. Default 600 when a job is created. A running job with no applied heartbeat for `ttl` seconds is stalled. |

`200` and `{"ok":true,"applied":true}` means at least one item changed the card. `applied: false` is still success: the post was an equal or older `seq`.

Do not post `stalled` or `abandoned`. Those words are not states. The board derives them when it reads. Posting either word is rejected.

## When to post which state

- `queued` when the work is accepted and not started.
- `running` while you are doing it. Heartbeat before `ttl` (default 10 minutes) with a higher `seq`, or with `seq` omitted on raw HTTP. An equal seq does not refresh the clock, so the job can stall even though you posted. `statuspan run` heartbeats for you.
- `blocked` only when a person has to answer or approve before you can continue. Say why in `note`. This shows under Needs attention.
- `done` when the finish line is reached. That clears Needs attention.
- `failed` when it stopped unsuccessfully. This also shows under Needs attention.

`stalled` is only for `running` past `ttl`. Abandoned comes later: `max(24h, 4 * ttl)` for running, and 7 days for queued or blocked. Done and failed are never abandoned. A newer applied post revives an abandoned job.

## Job ids

Use a stable slug such as `build-1` or `docs-refresh`. One slug per piece of work, reused for every heartbeat. A new slug is a new card. No secrets in `agent`, `job_id`, `title`, `note`, or `link`.

## More

- `statuspan agent` prints this skill from the binary you have installed.
- <https://statuspan.com/skill.md>
- <https://statuspan.com/llms.txt>
- <https://statuspan.com/llms-full.txt>
- <https://statuspan.com/openapi.json>
- <https://github.com/kirillzubovsky/statuspan>
