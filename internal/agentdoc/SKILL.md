---
name: statuspan
description: "Post job status to a local or self-hosted StatusPan board so a person can see which agent is working on what. Use when starting, updating, blocking, finishing, or failing a finite job, and when a recurring job becomes blocked or failed."
---

# StatusPan

StatusPan is a local airport-style board. You push. It does not call you back. The local daemon has no auth and listens only on loopback (`127.0.0.1:4477`). Another machine posts to a self-hosted relay instead.

## Install and run

Do these steps in order. This is enough to install StatusPan, start the board, and post a first job.

1. Install the CLI (macOS or Linux):

```bash
curl -fsSL https://statuspan.com/install.sh | sh
```

The installer puts the binary in `/usr/local/bin` when you are root or that directory is writable, otherwise in `~/.local/bin`. It does not edit your shell rc files.

2. Make sure `statuspan` is on PATH:

```bash
command -v statuspan
```

If that prints nothing, add the install directory to PATH, or call the binary by the full path the installer printed:

```bash
export PATH="/usr/local/bin:$HOME/.local/bin:$PATH"
command -v statuspan
# or run it by full path, for example:
#   ~/.local/bin/statuspan serve
#   /usr/local/bin/statuspan serve
```

To keep PATH for new shells, append that `export` line to `~/.bashrc` or `~/.zshrc`. Do not skip this check. `statuspan: command not found` means the install directory is not on PATH.

3. Start the board in the background. It listens only on loopback. The default address is `127.0.0.1:4477`. To use another loopback port (for example if 4477 is already taken), set `STATUSPAN_ADDR`. You can also set `STATUSPAN_DATA` to a separate data directory (default `~/.config/statuspan`):

```bash
export STATUSPAN_ADDR=127.0.0.1:4477
# throwaway port example: export STATUSPAN_ADDR=127.0.0.1:14477
statuspan serve &
```

`statuspan serve --addr 127.0.0.1:14477` is the same as setting `STATUSPAN_ADDR`. Leave the process running.

4. Health-check the board. If the connection is refused, wait a second and try again until you get JSON with a `jobs` array (empty is fine):

```bash
curl -s "http://${STATUSPAN_ADDR:-127.0.0.1:4477}/api/jobs"
```

Then open the board at `http://127.0.0.1:4477`, or at whatever host:port you set in `STATUSPAN_ADDR`.

5. Post a test job. If you set `STATUSPAN_ADDR`, `statuspan post` uses it automatically. You can also pass `--addr 127.0.0.1:14477`.

```bash
statuspan post --agent "My Agent" --job hello --state running --note "first post"
```

6. Confirm it shows on the board. Reload the page, or:

```bash
curl -s "http://${STATUSPAN_ADDR:-127.0.0.1:4477}/api/jobs"
statuspan ls
```

You should see My Agent / hello as running.

7. Install the agent skill so later sessions can post without this guide:

```bash
statuspan agent install
```

8. Optional: if this machine is not the one running the board, connect it to your relay instead of posting to localhost. See [Connect another machine to your relay](#connect-another-machine-to-your-relay) below, or <https://statuspan.com/relay>.


The board is optional. A failed, slow, or unreachable StatusPan must never block, delay, or fail the real work. Time out in about 2 seconds and ignore errors (`curl --max-time 2 ... || true`).

Full detail: <https://statuspan.com/llms-full.txt>. Machine-readable: `statuspan agent json`. This file is enough to post correctly.

On a Mac, [StatusPan.dmg](https://github.com/kirillzubovsky/statuspan/releases/latest/download/StatusPan.dmg) is a WKWebView window on the local board. It starts the bundled `statuspan serve` if nothing answers on `STATUSPAN_ADDR` (default `127.0.0.1:4477`), and it stops only a daemon it started.

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

If you are Claude Code and `statuspan hooks install claude` has been run, your session is reported automatically; still post meaningful titles and progress yourself when useful. The hook only posts status. It does not print into the session and it does not block.

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

## Off this machine

A cloud or remote agent (Cursor cloud agents, CI, another server) cannot reach `127.0.0.1` on your machine. It posts to a relay with a write token stored as a secret. Your local daemon pulls those jobs in. Hosted Relay needs no Cloudflare setup: <https://statuspan.com/cloud>. Full steps: <https://statuspan.com/relay> and <https://statuspan.com/llms-full.txt>.

Self-host from `relay/` with a free Cloudflare account and Node.js. `wrangler deploy` prints the `workers.dev` URL. Never commit `admin-token`.

```bash
cd relay
npm install
npx wrangler login
npx wrangler deploy
openssl rand -hex 32 > admin-token
npx wrangler secret put ADMIN_TOKEN < admin-token
chmod 600 admin-token
export RELAY_URL=https://statuspan-relay.<account>.workers.dev
export ADMIN_TOKEN=$(cat admin-token)
node scripts/admin.mjs create-workspace home
node scripts/admin.mjs mint home --kind write --agent MyBot --label laptop --out write-token
node scripts/admin.mjs mint home --kind read --label board --out read-token
chmod 600 write-token read-token
```

A write token posts for one agent and cannot read the board. A read token feeds the board and cannot post. The admin token creates workspaces and mints, lists, and revokes tokens. Never give it to bots. Point the daemon at the relay with `relay.json` (macOS: `~/Library/Application Support/statuspan/relay.json`) or `STATUSPAN_RELAY_URL`, `STATUSPAN_RELAY_WORKSPACE`, and `STATUSPAN_RELAY_READ_TOKEN`. Agents post with `STATUSPAN_RELAY_URL`, `STATUSPAN_RELAY_WORKSPACE`, and `STATUSPAN_TOKEN_FILE` (or `STATUSPAN_TOKEN`).

## Connect another machine to your relay

Use this when a second computer, a cloud agent, or CI should post to the same board. Those agents cannot reach `127.0.0.1` on the machine that runs the board.

Token roles:

| Token | Role |
| --- | --- |
| write | Per machine or agent. Post only. Cannot read the board. |
| read | Local daemon and board only. Cannot post. |
| admin | Mint and revoke tokens, create workspaces. Never give it to bots. |

1. On the machine that has the admin token, from `relay/`:

```bash
export RELAY_URL="https://<your-relay>.workers.dev"
export ADMIN_TOKEN=$(cat admin-token)
node scripts/admin.mjs mint <workspace> --kind write --agent <agent-name> --label <machine-label> --out write-token
```

The command is `node scripts/admin.mjs mint <workspace> --kind read|write [--agent <name>] [--label <label>] [--out <path>]`. A write token requires `--agent`. `--label` is optional and should name the machine. `--out` writes only the token (mode 0600) and prints `saved to <path>`. The token is scoped to that agent name; posts from the new machine must use the same `--agent`. `RELAY_URL` and `ADMIN_TOKEN` come from the environment, or `--relay-url` / `--admin-token-file`.

2. On the new machine, save the token to `~/.config/statuspan/token` (`chmod 600`). Never put the token in shell history or chat. Prefer copying the file minted with `--out`, or pasting it into an editor. Do not `echo` the token on the command line.

```bash
mkdir -p ~/.config/statuspan
# copy write-token onto the new machine as ~/.config/statuspan/token
chmod 600 ~/.config/statuspan/token
```

3. On the new machine, set these and persist them in your shell rc (`~/.bashrc` or `~/.zshrc`):

```bash
export STATUSPAN_RELAY_URL="https://<your-relay>.workers.dev"
export STATUSPAN_RELAY_WORKSPACE="<workspace>"
export STATUSPAN_TOKEN_FILE="$HOME/.config/statuspan/token"
```

4. Verify. Post a test job (the agent name must match the token) and check the local board within a few seconds:

```bash
statuspan post --agent "<agent-name>" --job connect-test --state running --note "from the new machine"
```

Then look for that job on the board, or `curl -s "http://${STATUSPAN_ADDR:-127.0.0.1:4477}/api/jobs"`.

## Where did this update come from?

A hostname in a note is self-reported. The agent typed it, and a client can fake it.

The relay stamps each accepted write with the write token's label (or the token id when the label is empty), the request IP, the country, and the time it received the update. Clients cannot set or overwrite `via`, `ip`, `country`, or `received_at`. The board shows a muted line such as `via ci-builder · 203.0.113.10 · US`. A post that never went through the relay shows `via local`.

The local board only shows this after it is running a daemon build that understands the new fields. An older daemon still syncs jobs; it just will not display the stamp.

## More

- `statuspan agent` prints this skill from the binary you have installed.
- <https://statuspan.com/skill.md>
- <https://statuspan.com/llms.txt>
- <https://statuspan.com/llms-full.txt>
- <https://statuspan.com/relay>
- <https://statuspan.com/cloud>
- <https://statuspan.com/openapi.json>
- <https://github.com/kirillzubovsky/statuspan>
