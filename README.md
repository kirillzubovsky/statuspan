# StatusPan

Local board for which AI agent is working on what. One Go binary, loopback only, append-only JSONL log. MIT license (see [LICENSE](LICENSE)).

A job update has `agent`, `job_id`, and `state` (`queued`, `running`, `blocked`, `done`, `failed`). Later updates may omit `state`. When `seq` is set, the highest seq wins. An older seq stays in history and does not change the job. Bots cannot post `abandoned`. That word is not a stored state.

StatusPan is one-way. Bots push updates. StatusPan never pushes back: no owner nudges, no webhooks, and no board buttons that edit a job's state. Jobs are removed by Clear all, which asks first, or by a clear control on one section. A local-board option can overlay a shorter display title. It does not change the job or nudge the agent.

A running job with no heartbeat for `ttl` seconds (default 600) is stalled. Queued, blocked, done, and failed are not. Stalled and abandoned are derived when the board is read. They are not written into the JSONL log.

Abandoned is the next step after a long silence:

- A running job is abandoned only after it is already stalled, once the silence is longer than `max(abandon-after, 4 * ttl)`. The default `abandon-after` is 24h. With `ttl` 7200 (2h), `4 * ttl` is 8h, so the job stalls at 2h and is abandoned at 24h. With a large `ttl`, `4 * ttl` wins when it is longer than 24h.
- A queued or blocked job does not stall. It is abandoned after 7 days of silence (`abandon-waiting-after`, default 168h). That window is not multiplied by `ttl`.
- Done and failed are never abandoned.
- At the exact threshold the job is not abandoned yet. One instant later it is. `abandoned_at` is `updated_at` plus the threshold that was crossed.
- An abandoned job is not stalled. It leaves the Running, Queued, or Blocked group and shows under Abandoned.
- A new applied post revives it. `updated_at` moves and abandoned is computed again, so the job returns to its posted state. An older `seq` stays in history and does not revive it.
- Set either window to `0` to disable that category. Set both to `0` to disable auto-abandon. The launchd plist does not set these; the defaults are enough.

`statuspan serve` flags, both Go durations (`24h`, `168h`, `0`): `--abandon-after` / `STATUSPAN_ABANDON_AFTER` (default `24h`), `--abandon-waiting-after` / `STATUSPAN_ABANDON_WAITING_AFTER` (default `168h`). The flag wins over the environment variable. `GET /api/jobs` and the SSE snapshot include `abandon_after_seconds` and `abandon_waiting_after_seconds` (`0` means that category is off) so the board can derive the same answer between events. `statuspan ls` prints `ABANDONED` instead of `STALLED` when a job is abandoned.

## What belongs on the board

StatusPan tracks dispatched jobs that have a finish line. Healthy recurring jobs and scheduled routines stay off the board. A daily or hourly run that is going fine does not get a card.

If a recurring job becomes `blocked` (it needs human input or approval) or `failed`, push it. It then shows under Needs attention. Once it is fixed or unblocked, push `done` so it clears out of Needs attention.

A routine can still post a separate job when it produces real, finishable work (for example an inbox check that found a reply to handle). A quiet routine run never appears.

Wrap a recurring command with `statuspan run --recurring`. It posts nothing while the command runs and nothing on exit 0. A non-zero exit posts one `failed`, which shows under Needs attention. Post `done` yourself once it is resolved.

The relay in `relay/` is an optional Cloudflare Worker. Bots that are not on this machine post there. The daemon polls it and merges events into the local board. The same JSON body works for `POST /api/jobs` on the daemon and `POST /v1/w/<workspace>/jobs` on the relay.

## Install

The CLI installer works on macOS and Linux and installs to `~/.local/bin/statuspan`. No sudo.

```bash
curl -fsSL https://statuspan.com/install.sh | sh
```

### Mac app

Download [StatusPan.dmg](https://github.com/kirillzubovsky/statuspan/releases/latest/download/StatusPan.dmg). StatusPan.app opens the local board in its own window. If nothing answers on `STATUSPAN_ADDR` (default `127.0.0.1:4477`), it starts the bundled `statuspan serve`. Quitting the app stops only a daemon it started.

Build from source with `macos/build.sh` (for example `macos/build.sh 0.0.0-dev`). That writes `macos/.build/StatusPan.app` and `macos/.build/StatusPan.dmg`. See [macos/README.md](macos/README.md).

The release attaches `StatusPan.dmg` along with the CLI binaries.

## Local only

`make build` writes `./statuspan`. Defaults are `127.0.0.1:4477` and `~/Library/Application Support/statuspan` (`--addr` / `STATUSPAN_ADDR`, `--data` / `STATUSPAN_DATA`). Non-loopback addresses are refused.

```bash
./statuspan serve
```

Open http://127.0.0.1:4477 (`/` or `#/`). The home page is an airport-style board: Needs attention (failed, blocked, stalled, and abandoned), then In progress. Queued and Done stay folded until you open them. Each visible section header has its own clear: Clear on Needs attention, In progress, and Queued, and Clear done on Done. Clear all stays in the top menu. Coming back to the window refreshes the board. If the connection is stale, the header shows an offline / last-updated warning instead of the live clock. Click a row to open that job (`/#/job/<agent>/<job_id>`). The bot name in the job details opens that bot's jobs and timeline (`/#/agent/<name>`). Press `T`, or the top-bar button, to toggle day and night.

The ⋯ menu includes Smart job titles. It is off by default. Turning it on asks you to confirm a one-time download of an open-source model (Qwen 2.5 0.5B, about 500 MB). The daemon caches that model in the data directory. The board rewrites long titles on this machine only. It does not edit job state, nudge an agent, or send the title anywhere else. Turning it off stops new rewrites and leaves titles that were already shortened.

`./statuspan post` posts to the relay when `STATUSPAN_RELAY_URL`, `STATUSPAN_RELAY_WORKSPACE`, and `STATUSPAN_TOKEN_FILE` are set, and to the local daemon otherwise.

```bash
./statuspan post --agent MyBot --job build-1 --state running --title "Build v1" --progress 40
./statuspan post --agent MyBot --job build-1 --progress 55 --note halfway
curl -s 'localhost:4477/api/events?agent=MyBot'
curl -N localhost:4477/api/stream
./statuspan ls
./statuspan clear --yes
./statuspan clear --yes --section done
```

As a backup, you can always use the URL with plain curl, as long as the URL is the one where StatusPan is actually running.

```bash
curl -s -X POST "${STATUSPAN_URL:-http://127.0.0.1:4477/api/jobs}" -H 'Content-Type: application/json' -d '{"agent":"MyBot","job_id":"build-1","title":"Build v1","state":"running","progress":40,"link":"https://example.com","seq":1,"ttl":600}'
curl -s -X POST "${STATUSPAN_URL:-http://127.0.0.1:4477/api/jobs}" -H 'Content-Type: application/json' -d '{"agent":"MyBot","job_id":"build-1","progress":80,"note":"almost there"}'
```

`statuspan clear --yes` deletes every job and its history through the local daemon (`--addr` / `STATUSPAN_ADDR`). `statuspan clear --yes --section done` (or `attention`, `progress`, `queued`) deletes only that section. Without `--yes` it changes nothing. An unknown `--section` is an error and does not call the daemon.

`DELETE /api/jobs?confirm=all` wipes the board. A JSON body `{"confirm":"all"}` is also accepted. The query wins over the body. Before the wipe, the daemon copies the log to `jobs-backup-<unix>.jsonl` in the data directory. The response is `{"ok":true,"deleted":N,"backup":"..."}`.

`DELETE /api/jobs?confirm=done` (or `attention`, `progress`, `queued`) deletes only the jobs in that section and leaves the rest of the log. There is no backup. The response is `{"ok":true,"deleted":N,"section":"done"}`. Sections match the board: attention is failed, blocked, stalled, and abandoned (including an abandoned queued or blocked job); progress is a healthy running job; queued is queued and not abandoned; done is done. A missing or any other confirmation is a 400 (`confirm must be "all", "attention", "progress", "queued", or "done"`) and leaves the board alone. Invalid JSON is `400` `invalid json`.

launchd, from this repo:

```bash
make build
mkdir -p "$HOME/.local/bin" "$HOME/Library/Logs"
cp ./statuspan "$HOME/.local/bin/statuspan"
sed "s|__HOME__|$HOME|g" contrib/launchd/com.statuspan.daemon.plist > "$HOME/Library/LaunchAgents/com.statuspan.daemon.plist"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.statuspan.daemon.plist"
```

The plist listens on `127.0.0.1:4477` and writes `~/Library/Logs/statuspan.log`. With no relay flags, no relay environment variables, and no `relay.json`, the daemon never calls out.

`GET /healthz` is JSON: `ok` and `relay` (`configured`, `last_ok`, `cursor`, `last_error`). The token is not included. `configured` is false in local-only mode.

## statuspan run

`statuspan run` wraps a command and reports it. Posts before the command finishes are fire-and-forget. If StatusPan is unreachable, the command still runs and StatusPan exits with that command's status. A down board is a warning on stderr only with `--verbose`. A relay or endpoint misconfiguration prints one warning, skips posting, and still runs the command. The final post is sent with a short timeout before StatusPan exits. The token is not printed.

```bash
# example: wrap any command
statuspan run --agent "My Agent" --job build-1 --title "Build v1" -- make test
```

It posts `running` at progress 0, heartbeats, then `done` at progress 100 when the command exits 0 (`Completed in <duration>`). A non-zero exit posts `failed` (`Exited with code N after <duration>`). A signal is `128` plus the signal number. The child stays in StatusPan's process group, so an interactive command keeps the terminal. StatusPan catches SIGINT and SIGTERM, forwards SIGTERM to the child, and forwards SIGINT only when stdin is not a terminal. If the command cannot be started, both modes post `failed` (`Could not start: ...`) and StatusPan exits 127.

`--ttl` is the stall window in seconds (default 600). Heartbeats default to `min(ttl/2, 60s)` and are at least 10 seconds apart. `--heartbeat SEC` sets the interval with no floor.

`--recurring` is the quiet form of the rule above: no posts while the command runs, and no post on exit 0. A non-zero exit posts one `failed`.

`--agent` defaults to `STATUSPAN_AGENT`. The title defaults to the job id. `--link` is optional. Omit `--seq` and the CLI sends epoch milliseconds, strictly increasing in this process. `post` does the same when `--seq` is omitted.

The endpoint is the local daemon (`--addr` / `STATUSPAN_ADDR`, default `127.0.0.1:4477`) unless `--url` or `STATUSPAN_URL` sets the full jobs URL (for example `http://127.0.0.1:14477/api/jobs`). A relay uses the same flags and environment as `post`: `--relay` / `STATUSPAN_RELAY_URL`, `--workspace` / `STATUSPAN_RELAY_WORKSPACE`, and `--token` / `STATUSPAN_TOKEN` or `--token-file` / `STATUSPAN_TOKEN_FILE`. The write token is sent as `Authorization: Bearer` and is not printed. `post` accepts `--url` / `STATUSPAN_URL` and `STATUSPAN_TOKEN_FILE` the same way. On `post`, an incomplete relay, an unreadable token file, a bad `--url` or `--addr`, or `--url` combined with a relay is an error and does not post locally. On `run`, that same misconfiguration prints one warning and the command still runs, with no posts.

## Agent hooks

Claude Code, Grok, Codex, Cursor, and Gemini CLI can report to the board from lifecycle hooks, even when the model does not post. A prompt sets the title, a tool call updates the note, a permission prompt marks the job blocked, the end of a turn marks it done, and the end of the session marks it done. The hooks only post status.

| Tool | Install | File |
| --- | --- | --- |
| Claude Code | `statuspan hooks install claude` | `~/.claude/settings.json` |
| Grok | `statuspan hooks install grok` | `~/.grok/hooks/statuspan.json` |
| Codex | `statuspan hooks install codex` | `~/.codex/hooks.json` |
| Cursor | `statuspan hooks install cursor` | `~/.cursor/hooks.json` |
| Gemini CLI | `statuspan hooks install gemini` | `~/.gemini/settings.json` |

```bash
statuspan hooks install claude
statuspan hooks install grok
statuspan hooks install codex
statuspan hooks install cursor
statuspan hooks install gemini
statuspan hooks install claude --project
statuspan hooks install grok --project
statuspan hooks install codex --project
statuspan hooks install cursor --project
statuspan hooks install gemini --project
statuspan hooks install claude --dry-run
statuspan hooks uninstall claude
statuspan hooks uninstall grok
statuspan hooks uninstall codex
statuspan hooks uninstall cursor
statuspan hooks uninstall gemini
```

`--project` writes `./.claude/settings.json`, `./.grok/hooks/statuspan.json`, `./.codex/hooks.json`, `./.cursor/hooks.json`, or `./.gemini/settings.json` (`--dir` chooses the project root). `--settings PATH` chooses a file. `--home PATH` overrides the home directory. `--dry-run` prints the JSON and writes nothing. Secrets in titles and notes are redacted best-effort before the post. Stop, or the equivalent turn-end hook, runs at the end of every turn, so an idle interactive session shows done until the next prompt. Claude and Grok print nothing back into the session. Codex prints `{}` on stdout, because plain text would be added as developer context and Stop requires JSON. Cursor prints `{"continue":true}` for beforeSubmitPrompt and `{}` for every other event. Gemini prints `{}` on stdout, because the CLI accepts JSON only. The hooks only post status and do not block the session. Do not pass `--token` to `hooks install`. Use `--token-file` or `STATUSPAN_TOKEN`.

Claude Code merges handlers into the settings file and leaves other hooks in place. SessionStart is synchronous with a 3 second timeout so its post lands before later events. The other Claude hooks are asynchronous. Codex merges the same way into `hooks.json`. Its SessionStart, Stop, Interrupt, and SessionEnd handlers are synchronous. UserPromptSubmit, PreToolUse, and PermissionRequest are asynchronous. `hooks install codex` uses `$CODEX_HOME/hooks.json` when `CODEX_HOME` is set and `--home` is not. Codex needs `/hooks` trust before those hooks run. Trust is per hook hash, so installing again with different flags needs trusting again. Codex project hooks load only in trusted projects.

Grok writes one StatusPan-owned file and loads every `hooks/*.json`. `hooks install grok` refuses to overwrite a file that is not already a StatusPan `hook grok` command. `hooks uninstall grok` removes only that file. Grok project hooks need folder trust (`/hooks-trust` or `--trust`) before they run.

Grok also reads `~/.claude/settings.json` and `~/.cursor/hooks.json` and reports those sessions as Grok. When `GROK_HOOK_EVENT` is set, StatusPan reports the session as Grok, so a machine with both installs posts the same Grok update twice. That second post is harmless.

Cursor uses only non-blocking hooks. It merges command hooks into `~/.cursor/hooks.json`. The Cursor IDE agent and the Cursor CLI both read that file. Project hooks live at `<dir>/.cursor/hooks.json` and run from the project root. The file is flat: `version` stays `1`, and each event is an array of `{command, timeout}` objects with no matcher. Other hooks and keys are left in place. A second install replaces the StatusPan handlers. Uninstall removes only a `hook cursor` command, drops events that become empty, and keeps the file. Cursor watches `hooks.json` and reloads it automatically. StatusPan does not register Cursor permission hooks (`preToolUse`, `beforeShellExecution`, `beforeMCPExecution`, `beforeReadFile`, `subagentStart`). Invalid JSON blocks those, and an allow reply can pre-approve a tool. Ask mode is not posted. A stop hook never sends `followup_message`.

Gemini CLI merges handlers into `~/.gemini/settings.json` and leaves every other settings key in place, including `hooksConfig`. `--project` writes `<dir>/.gemini/settings.json`. Each handler is named `statuspan`. Timeouts are milliseconds: SessionStart, BeforeAgent, BeforeTool, Notification, and SessionEnd use 3000, and AfterAgent uses 5000. BeforeTool uses the matcher `.*`. A second install replaces the StatusPan handlers. Uninstall removes only a `hook gemini` command and drops the `hooks` key when nothing else remains. `hooksConfig.enabled` is left as it is. Stdout is `{}` because the CLI accepts JSON only. SessionEnd is best-effort: the CLI does not wait for it.

## statuspan agent

The binary carries the instructions an agent needs.

```bash
statuspan agent            # print the skill
statuspan agent prompt     # short snippet for AGENTS.md or CLAUDE.md
statuspan agent json       # discovery document
statuspan agent install    # install the skill
statuspan skill            # alias of statuspan agent
```

`statuspan agent install` writes `~/.agents/skills/statuspan/SKILL.md`. It also writes `~/.claude/skills/statuspan/SKILL.md`, `~/.cursor/skills/statuspan/SKILL.md`, and `~/.codex/skills/statuspan/SKILL.md` when that directory already exists. `--target` is `auto` (the default), `agents`, `claude`, `cursor`, `codex`, `all`, or a comma-separated list. An explicit target is created even when the directory is new. `--dir PATH` writes `PATH/SKILL.md` instead of those targets. `--home PATH` overrides the home directory. `--dry-run` prints each path and writes nothing.

## Self-host the relay

`relay/` is a Worker with one SQLite Durable Object per workspace. A free Cloudflare account is enough: `workers.dev` and SQLite Durable Objects are on the free plan. You also need Node.js.

```bash
cd relay
npm install
npx wrangler login
npx wrangler deploy
openssl rand -hex 32 > admin-token
npx wrangler secret put ADMIN_TOKEN < admin-token
chmod 600 admin-token
```

`wrangler deploy` prints the `workers.dev` URL. Keep `admin-token` private. Do not commit it.

Create a workspace and mint tokens. `RELAY_URL` is that URL. `ADMIN_TOKEN` is the secret you just set.

```bash
export RELAY_URL=https://statuspan-relay.<account>.workers.dev
export ADMIN_TOKEN=$(cat admin-token)
node scripts/admin.mjs create-workspace home
node scripts/admin.mjs mint home --kind write --agent MyBot --label laptop --out write-token
node scripts/admin.mjs mint home --kind read --label board --out read-token
chmod 600 write-token read-token
```

`--out` writes only the token (mode 0600) and prints `saved to <path>`.

A write token belongs to one agent. It can post updates for that agent and cannot read jobs, events, or the stream. A read token can list jobs, poll events, and open the stream, and it cannot post. The admin token creates workspaces and mints, lists, and revokes tokens. Bots do not send it.

Point the daemon at the relay with `relay.json` in the data directory (`~/Library/Application Support/statuspan/relay.json`):

```json
{"url":"https://statuspan-relay.<account>.workers.dev","workspace":"home","read_token":"<paste the read token>"}
```

`chmod 600` that file. Restart the daemon. On start, and about every 3 seconds after that, it calls `GET /v1/w/home/events?since=<cursor>`, merges each event with the same seq rules as a local post, and saves the cursor in `relay-cursor` so a restart resumes. Failures back off, with jitter, up to about 60 seconds. Errors are logged without the token. Merged jobs go out on the existing SSE stream, so the board updates live.

Flags win over the environment, which wins over `relay.json`:

- `--relay-url`, `--relay-workspace`, `--relay-token-file`
- `STATUSPAN_RELAY_URL`, `STATUSPAN_RELAY_WORKSPACE`, `STATUSPAN_RELAY_READ_TOKEN`

If the URL, workspace, or read token is set and another of the three is missing, the daemon exits with an error instead of falling back to local-only.

`--relay-token-file` is a read token, one line, trimmed. `STATUSPAN_RELAY_READ_TOKEN` is that same read token. The CLI write token is a different variable, below.

A bot posts with its write token:

```bash
curl -s -X POST "$RELAY_URL/v1/w/home/jobs" \
  -H "Authorization: Bearer $(cat write-token)" \
  -H 'Content-Type: application/json' \
  -d '{"agent":"MyBot","job_id":"build-1","title":"Build v1","state":"running","progress":40,"seq":1}'
```

Or the CLI, which posts to the relay instead of the local daemon when these are set:

```bash
./statuspan post --relay "$RELAY_URL" --workspace home --token-file write-token \
  --agent MyBot --job build-1 --state running --progress 55 --note halfway
```

`--token` passes the write token on the command line. The environment names are `STATUSPAN_RELAY_URL`, `STATUSPAN_RELAY_WORKSPACE`, and `STATUSPAN_TOKEN`. Leave all of them unset to post to the local daemon. If only some are set, `post` fails and does not post locally. `run` prints one warning and still runs the wrapped command, without posting. A relay HTTP error on `post` prints the status and the relay's message.

Read with the read token:

```bash
curl -s "$RELAY_URL/v1/w/home/jobs" -H "Authorization: Bearer $(cat read-token)"
curl -s "$RELAY_URL/v1/w/home/events?since=0" -H "Authorization: Bearer $(cat read-token)"
```

List and revoke. `list` prints token ids, not the secrets:

```bash
node scripts/admin.mjs list home
node scripts/admin.mjs revoke home <token-id>
```

## Hosted StatusPan (coming soon, paid)

We will run the relay, so you do not have to manage Cloudflare. Same open protocol: a write token posts updates, a read token feeds your local daemon. You can always self-host instead.
