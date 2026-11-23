# StatusPan

Local board for which AI agent is working on what. One Go binary, loopback only, append-only JSONL log. MIT license (see [LICENSE](LICENSE)).

A job update has `agent`, `job_id`, and `state` (`queued`, `running`, `blocked`, `done`, `failed`). Later updates may omit `state`. When `seq` is set, the highest seq wins. An older seq stays in history and does not change the job. Bots cannot post `abandoned`. That word is not a stored state.

StatusPan is one-way. Bots push updates. StatusPan never pushes back: no owner nudges, no webhooks, and no board buttons that edit a job. Clear all is the one action that removes jobs, and it asks first.

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

StatusPan tracks dispatched jobs that have a finish line. Recurring routines and watchers do not get their own cards. A routine run posts a job only when it produces real, finishable work (for example an inbox check that found a reply to handle). A quiet routine run never appears.

The relay in `relay/` is an optional Cloudflare Worker. Bots that are not on this machine post there. The daemon polls it and merges events into the local board. The same JSON body works for `POST /api/jobs` on the daemon and `POST /v1/w/<workspace>/jobs` on the relay.

## Local only

`make build` writes `./statuspan`. Defaults are `127.0.0.1:4477` and `~/Library/Application Support/statuspan` (`--addr` / `STATUSPAN_ADDR`, `--data` / `STATUSPAN_DATA`). Non-loopback addresses are refused.

```bash
./statuspan serve
```

Open http://127.0.0.1:4477 (`/` or `#/`). The home page is an airport-style board: Needs attention (failed, blocked, stalled, and abandoned), then In progress. Queued and Done stay folded until you open them. Coming back to the window refreshes the board. If the connection is stale, the header shows an offline / last-updated warning instead of the live clock. Click a row to open that job (`/#/job/<agent>/<job_id>`). The bot name in the job details opens that bot's jobs and timeline (`/#/agent/<name>`). Press `T`, or the top-bar button, to toggle day and night.

```bash
curl -s -X POST localhost:4477/api/jobs -H 'Content-Type: application/json' -d '{"agent":"MyBot","job_id":"build-1","title":"Build v1","state":"running","progress":40,"link":"https://example.com","seq":1,"ttl":600}'
curl -s -X POST localhost:4477/api/jobs -H 'Content-Type: application/json' -d '{"agent":"MyBot","job_id":"build-1","progress":80,"note":"almost there"}'
./statuspan post --agent MyBot --job build-1 --state running --title "Build v1" --progress 40
./statuspan post --agent MyBot --job build-1 --progress 55 --note halfway
curl -s 'localhost:4477/api/events?agent=MyBot'
curl -N localhost:4477/api/stream
./statuspan ls
./statuspan clear --yes
```

`statuspan clear --yes` deletes every job and its history through the local daemon (`--addr` / `STATUSPAN_ADDR`). Without `--yes` it changes nothing. `DELETE /api/jobs?confirm=all` does the same. A JSON body `{"confirm":"all"}` is also accepted. Any other confirmation is a 400 and leaves the board alone. Before the wipe, the daemon copies the log to `jobs-backup-<unix>.jsonl` in the data directory.

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

`--token` passes the write token on the command line. The environment names are `STATUSPAN_RELAY_URL`, `STATUSPAN_RELAY_WORKSPACE`, and `STATUSPAN_TOKEN`. Leave all of them unset to post to the local daemon. If only some are set, the command fails and does not post locally. A relay error prints the HTTP status and the relay's message.

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
