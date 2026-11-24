---
name: statuspan
description: "Post job status to a local or self-hosted StatusPan board so a person can see which agent is working on what. Use when starting, updating, blocking, finishing, or failing a job with a finish line, and when a recurring job becomes blocked or failed."
---

# StatusPan

StatusPan is a local airport-style board. You push. It does not call you back. The local daemon has no auth and listens only on loopback. Another machine posts to a self-hosted relay instead.

Full detail: <https://statuspan.com/llms-full.txt>. This file is enough to post correctly.

## Install and serve

```bash
curl -fsSL https://statuspan.com/install.sh | sh
statuspan serve
```

Board: <http://127.0.0.1:4477>. Default listen address is `127.0.0.1:4477`.

## Post

`POST http://127.0.0.1:4477/api/jobs` with `Content-Type: application/json`. One object, or an array of objects. `200` and `{"ok":true,"applied":true}` means at least one item changed the current job. `applied: false` is still success: the post was an equal or older `seq` and did not change the card.

```bash
curl --max-time 2 -s -X POST http://127.0.0.1:4477/api/jobs \
  -H 'Content-Type: application/json' \
  -d '{"agent":"MyBot","job_id":"build-1","title":"Build v1","state":"running","progress":40,"seq":1}' \
  || true
```

A new job needs `agent`, `job_id`, and `state`. Later posts need `agent` and `job_id` and may omit everything else. Omitted fields are left as they are. Send a field as an empty string only when you mean to clear `title`, `link`, or `note`.

| Field | Rule |
| --- | --- |
| `agent` | Required. Not blank. No `/`, `\`, or NUL. |
| `job_id` | Required. Same rules. The card is `(agent, job_id)`. Reuse the slug for every update of that work. |
| `state` | `queued`, `running`, `blocked`, `done`, or `failed`. Required on a new job. Omit later to leave it. |
| `title` | Short label. Optional. |
| `progress` | Integer 0 through 100. Optional. |
| `link` | Any string, often an `https://example.com/...` URL. Optional. Not validated as a URL. |
| `note` | One line of status. Optional. |
| `seq` | Optional integer. Omit it and the post applies. If you set it, only a strictly greater seq changes the job. |
| `ttl` | Optional seconds. Default 600 on create. A running job with no applied heartbeat for `ttl` seconds is stalled. |

Do not post `stalled` or `abandoned`. Those words are not states. The board derives them when it reads.

## Fire and forget

A post must not block the real work. Timeout at 2 seconds and ignore failure, including connection refused.

```python
import json
import urllib.request

def statuspan_post(update):
    req = urllib.request.Request(
        "http://127.0.0.1:4477/api/jobs",
        data=json.dumps(update).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=2) as resp:
            resp.read()
    except Exception:
        pass
```

```javascript
async function statuspanPost(update) {
  try {
    await fetch("http://127.0.0.1:4477/api/jobs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(update),
      signal: AbortSignal.timeout(2000),
    });
  } catch {
    // The board is optional. Keep working.
  }
}
```

The same JSON works on a self-hosted relay. Post to `https://relay.example.com/v1/w/<workspace>/jobs` (or `https://statuspan-relay.<your-account>.workers.dev`) with `Authorization: Bearer <write token>`. A write token starts with `spw_` and can post for one agent only. It cannot read. A read token starts with `spr_` and cannot post. Do not invent a token. If you were not given one, post to the local daemon.

CLI, when the daemon is already running:

```bash
statuspan post --agent MyBot --job build-1 --state running --title "Build v1" --progress 40 --seq 1
```

## When to post which state

- `queued` when the work is accepted and not started.
- `running` while you are doing it. Heartbeat before `ttl` (default 10 minutes) with a higher `seq`, or with `seq` omitted. An equal seq does not refresh the clock, so the job can stall even though you posted.
- `blocked` when a person has to approve or answer before you can continue. This shows under Needs attention.
- `done` when the finish line is reached.
- `failed` when it stopped unsuccessfully. This also shows under Needs attention.

`statuspan ls` and the board read `stalled` only for `running` past `ttl`. Abandoned comes later: `max(24h, 4 * ttl)` for running, and 7 days for queued or blocked. Done and failed are never abandoned. A newer applied post revives an abandoned job.

## Recurring jobs

Healthy recurring jobs and scheduled routines stay off the board. Do not post a card for a daily or hourly run that is going fine.

If a recurring job becomes `blocked` or `failed`, you should push it so it surfaces in Needs attention. Once it is fixed or unblocked, push `done` so it clears out of Needs attention.

A routine may still post a different job when it produces real, finishable work. A quiet run posts nothing.

## Job ids

Use a stable slug such as `build-1` or `docs-refresh`. One slug per piece of work, reused for every heartbeat. A new slug is a new card. No secrets in `agent`, `job_id`, `title`, `note`, or `link`.

## More

- <https://statuspan.com/llms-full.txt>
- <https://statuspan.com/openapi.json>
- <https://statuspan.com/schema/job-update.schema.json>
- <https://github.com/kirillzubovsky/statuspan>
