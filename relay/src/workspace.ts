import { DurableObject } from "cloudflare:workers";
import { randomHex, randomToken, sha256Hex } from "./auth";
import { json } from "./http";
import {
  MAX_EVENTS,
  MAX_JOB_HISTORY,
  freshJob,
  mergeJob,
  parseUpdates,
  seqApplies,
  stateRequiredMessage,
  toEventJson,
  toJobJson,
  validateId,
  validateUpdate,
  type EventRow,
  type JobRow,
  type ParsedUpdate,
} from "./protocol";

const EVENT_COLUMNS = `cursor, agent, job_id, state, has_progress, progress, note, title, link, ts, applied, has_seq, seq, kind, via, ip, country, received_at`;

const JOB_COLUMNS = `agent, job_id, title, state, progress, link, note, ttl, seq, has_seq, started_at, updated_at, finished_at, via, ip, country, received_at`;

type Provenance = {
  via: string;
  ip: string;
  country: string;
  received_at: string;
};

type TokenRow = {
  id: string;
  kind: string;
  agent: string | null;
  label: string | null;
  created: string;
};

type StreamSub = {
  send: (frame: string) => void;
  stop: () => void;
};

// One SQLite-backed object per workspace. idFromName(workspace) in the worker
// is the only routing key. Tokens live here as SHA-256 hex, never as plaintext.
export class Workspace extends DurableObject<Env> {
  private subs = new Set<StreamSub>();

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    ctx.blockConcurrencyWhile(async () => {
      this.migrate();
    });
  }

  async adminCreate(): Promise<Response> {
    const already = this.isCreated();
    if (!already) {
      this.sql.exec(
        "INSERT INTO meta (key, value) VALUES ('created', ?)",
        new Date().toISOString(),
      );
    }
    return json(200, { ok: true, created: !already });
  }

  async adminMint(body: string): Promise<Response> {
    if (!this.isCreated()) return notCreated();
    const parsed = parseMint(body);
    if ("error" in parsed) return json(400, { ok: false, error: parsed.error });
    const token = randomToken(parsed.kind);
    const hash = await sha256Hex(token);
    const id = this.newTokenId();
    const created = new Date().toISOString();
    this.sql.exec(
      `INSERT INTO tokens (id, hash, kind, agent, label, created) VALUES (?, ?, ?, ?, ?, ?)`,
      id,
      hash,
      parsed.kind,
      parsed.agent,
      parsed.label,
      created,
    );
    return json(200, {
      ok: true,
      id,
      token,
      kind: parsed.kind,
      agent: parsed.agent,
      label: parsed.label,
      created,
    });
  }

  async adminListTokens(): Promise<Response> {
    if (!this.isCreated()) return notCreated();
    const tokens = this.sql.exec<TokenRow>(
      "SELECT id, kind, agent, label, created FROM tokens ORDER BY created ASC, id ASC",
    ).toArray();
    return json(200, { ok: true, tokens });
  }

  async adminRevoke(id: string): Promise<Response> {
    if (!this.isCreated()) return notCreated();
    const found = this.sql.exec("SELECT id FROM tokens WHERE id = ?", id).toArray();
    if (found.length === 0) return json(404, { ok: false, error: "not found" });
    this.sql.exec("DELETE FROM tokens WHERE id = ?", id);
    return json(200, { ok: true, revoked: true });
  }

  async postJobs(
    tokenHash: string | null,
    body: string,
    net: { ip: string; country: string },
  ): Promise<Response> {
    const auth = this.gate(tokenHash, "write");
    if (auth instanceof Response) return auth;
    const parsed = parseUpdates(body);
    if ("error" in parsed) return json(400, { ok: false, error: parsed.error });
    for (const update of parsed.updates) {
      if (update.agent !== "" && update.agent !== auth.agent) {
        return json(403, { ok: false, error: "token is scoped to a different agent" });
      }
    }
    const invalid = this.validateBatch(parsed.updates);
    if (invalid) return json(400, { ok: false, error: invalid });
    const ts = new Date().toISOString();
    const stamp: Provenance = {
      via: tokenVia(auth),
      ip: (net?.ip ?? "").trim(),
      country: (net?.country ?? "").trim(),
      received_at: ts,
    };
    let applied = false;
    for (const update of parsed.updates) {
      if (this.applyUpdate(update, ts, stamp)) applied = true;
    }
    this.pruneEvents();
    return json(200, { ok: true, applied });
  }

  async listJobs(tokenHash: string | null): Promise<Response> {
    const auth = this.gate(tokenHash, "read");
    if (auth instanceof Response) return auth;
    const now = new Date().toISOString();
    return json(200, { now, jobs: this.jobViews(Date.parse(now)) });
  }

  async listEvents(
    tokenHash: string | null,
    since: number | "invalid",
    limit: number | "invalid",
  ): Promise<Response> {
    const auth = this.gate(tokenHash, "read");
    if (auth instanceof Response) return auth;
    if (since === "invalid") return json(400, { ok: false, error: "invalid since" });
    if (limit === "invalid") return json(400, { ok: false, error: "invalid limit" });
    const rows = limit <= 0 ? [] : this.sql.exec<EventRow>(
      `SELECT ${EVENT_COLUMNS} FROM events WHERE cursor > ? ORDER BY cursor ASC LIMIT ?`,
      since,
      limit,
    ).toArray();
    const events = rows.map(toEventJson);
    const last = events.length > 0 ? events[events.length - 1] : undefined;
    const nextCursor = typeof last?.cursor === "number" ? last.cursor : since;
    return json(200, {
      now: new Date().toISOString(),
      events,
      next_cursor: nextCursor,
    });
  }

  async stream(tokenHash: string | null): Promise<Response> {
    const auth = this.gate(tokenHash, "read");
    if (auth instanceof Response) return auth;
    const encoder = new TextEncoder();
    let sub: StreamSub | undefined;
    let timer: ReturnType<typeof setInterval> | undefined;
    let closed = false;
    const stop = () => {
      closed = true;
      if (timer !== undefined) clearInterval(timer);
      timer = undefined;
      if (sub) this.subs.delete(sub);
    };
    const stream = new ReadableStream<Uint8Array>({
      start: (controller) => {
        sub = {
          send: (frame: string) => {
            if (closed) return;
            try {
              controller.enqueue(encoder.encode(frame));
            } catch {
              stop();
            }
          },
          stop,
        };
        this.subs.add(sub);
        try {
          const now = new Date().toISOString();
          const snap = {
            now,
            jobs: this.jobViews(Date.parse(now)),
            events: this.recentEvents(200),
          };
          sub.send(`event: snapshot\ndata: ${JSON.stringify(snap)}\n\n`);
          if (closed) return;
          const current = sub;
          timer = setInterval(() => {
            current.send(": keepalive\n\n");
          }, 15_000);
        } catch (err) {
          stop();
          throw err;
        }
      },
      cancel: () => {
        stop();
      },
    });
    return new Response(stream, {
      headers: {
        "Content-Type": "text/event-stream; charset=utf-8",
        "Cache-Control": "no-cache, no-transform",
        "Connection": "keep-alive",
        "X-Accel-Buffering": "no",
      },
    });
  }

  private get sql(): SqlStorage {
    return this.ctx.storage.sql;
  }

  private migrate(): void {
    this.sql.exec(`CREATE TABLE IF NOT EXISTS meta (
      key TEXT PRIMARY KEY,
      value TEXT NOT NULL
    )`);
    this.sql.exec(`CREATE TABLE IF NOT EXISTS tokens (
      id TEXT PRIMARY KEY,
      hash TEXT NOT NULL UNIQUE,
      kind TEXT NOT NULL,
      agent TEXT,
      label TEXT,
      created TEXT NOT NULL
    )`);
    this.sql.exec(`CREATE TABLE IF NOT EXISTS jobs (
      agent TEXT NOT NULL,
      job_id TEXT NOT NULL,
      title TEXT NOT NULL,
      state TEXT NOT NULL,
      progress INTEGER NOT NULL,
      link TEXT NOT NULL,
      note TEXT NOT NULL,
      ttl INTEGER NOT NULL,
      seq INTEGER,
      has_seq INTEGER NOT NULL,
      started_at TEXT NOT NULL,
      updated_at TEXT NOT NULL,
      finished_at TEXT,
      via TEXT NOT NULL DEFAULT '',
      ip TEXT NOT NULL DEFAULT '',
      country TEXT NOT NULL DEFAULT '',
      received_at TEXT NOT NULL DEFAULT '',
      PRIMARY KEY (agent, job_id)
    )`);
    this.sql.exec(`CREATE TABLE IF NOT EXISTS events (
      cursor INTEGER PRIMARY KEY,
      agent TEXT NOT NULL,
      job_id TEXT NOT NULL,
      state TEXT NOT NULL,
      has_progress INTEGER NOT NULL,
      progress INTEGER,
      note TEXT NOT NULL,
      title TEXT NOT NULL,
      link TEXT NOT NULL,
      ts TEXT NOT NULL,
      applied INTEGER NOT NULL,
      has_seq INTEGER NOT NULL,
      seq INTEGER,
      kind TEXT NOT NULL,
      via TEXT NOT NULL DEFAULT '',
      ip TEXT NOT NULL DEFAULT '',
      country TEXT NOT NULL DEFAULT '',
      received_at TEXT NOT NULL DEFAULT ''
    )`);
    this.sql.exec(
      "CREATE INDEX IF NOT EXISTS events_job ON events (agent, job_id, cursor)",
    );
    // CREATE TABLE IF NOT EXISTS does not add columns to a Durable Object
    // that already has the table. Adding them again is a duplicate-column error.
    for (const column of ["via", "ip", "country", "received_at"] as const) {
      this.addTextColumn("jobs", column);
      this.addTextColumn("events", column);
    }
  }

  private addTextColumn(table: "jobs" | "events", column: "via" | "ip" | "country" | "received_at"): void {
    try {
      this.sql.exec(`ALTER TABLE ${table} ADD COLUMN ${column} TEXT NOT NULL DEFAULT ''`);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      if (msg.toLowerCase().includes("duplicate column")) return;
      throw err;
    }
  }

  private isCreated(): boolean {
    return this.sql.exec("SELECT 1 AS ok FROM meta WHERE key = 'created' LIMIT 1").toArray().length > 0;
  }

  private gate(tokenHash: string | null, need: "read" | "write"): Response | TokenRow {
    if (!this.isCreated()) return notCreated();
    if (!tokenHash) return json(401, { ok: false, error: "unauthorized" });
    const rows = this.sql.exec<TokenRow & { hash: string }>(
      "SELECT id, kind, agent, label, created FROM tokens WHERE hash = ?",
      tokenHash,
    ).toArray();
    const token = rows[0];
    if (!token || (token.kind !== "read" && token.kind !== "write")) {
      return json(401, { ok: false, error: "unauthorized" });
    }
    if (need === "read" && token.kind !== "read") {
      return json(403, { ok: false, error: "write token cannot read" });
    }
    if (need === "write" && token.kind !== "write") {
      return json(403, { ok: false, error: "read token cannot write" });
    }
    return token;
  }

  private validateBatch(updates: ParsedUpdate[]): string | null {
    const pending = new Set<string>();
    for (const update of updates) {
      const error = validateUpdate(update);
      if (error) return error;
      const key = jobKey(update.agent, update.jobId);
      if (update.state === "") {
        if (!this.jobExists(update.agent, update.jobId) && !pending.has(key)) {
          return stateRequiredMessage();
        }
      } else if (!this.jobExists(update.agent, update.jobId)) {
        pending.add(key);
      }
    }
    return null;
  }

  private applyUpdate(update: ParsedUpdate, ts: string, stamp: Provenance): boolean {
    const existing = this.loadJob(update.agent, update.jobId);
    const applied = seqApplies(existing, update.seq);
    const job = existing ?? freshJob(update, ts);
    const cursor = this.allocCursor();
    this.sql.exec(
      `INSERT INTO events (
        cursor, agent, job_id, state, has_progress, progress, note, title, link,
        ts, applied, has_seq, seq, kind, via, ip, country, received_at
      ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'update', ?, ?, ?, ?)`,
      cursor,
      update.agent,
      update.jobId,
      update.state,
      update.progress === undefined ? 0 : 1,
      update.progress ?? null,
      update.note ?? "",
      update.title ?? "",
      update.link ?? "",
      ts,
      applied ? 1 : 0,
      update.seq === undefined ? 0 : 1,
      update.seq ?? null,
      stamp.via,
      stamp.ip,
      stamp.country,
      stamp.received_at,
    );
    if (!applied) return false;
    mergeJob(job, update, ts);
    job.via = stamp.via;
    job.ip = stamp.ip;
    job.country = stamp.country;
    job.received_at = stamp.received_at;
    this.saveJob(job);
    this.publishJob(job);
    return true;
  }

  private loadJob(agent: string, jobId: string): JobRow | null {
    const rows = this.sql.exec<JobRow>(
      `SELECT ${JOB_COLUMNS} FROM jobs WHERE agent = ? AND job_id = ?`,
      agent,
      jobId,
    ).toArray();
    return rows[0] ?? null;
  }

  private jobExists(agent: string, jobId: string): boolean {
    return this.sql.exec(
      "SELECT 1 AS ok FROM jobs WHERE agent = ? AND job_id = ? LIMIT 1",
      agent,
      jobId,
    ).toArray().length > 0;
  }

  private saveJob(job: JobRow): void {
    this.sql.exec(
      `INSERT INTO jobs (
        agent, job_id, title, state, progress, link, note, ttl, seq, has_seq,
        started_at, updated_at, finished_at, via, ip, country, received_at
      ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
      ON CONFLICT(agent, job_id) DO UPDATE SET
        title = excluded.title,
        state = excluded.state,
        progress = excluded.progress,
        link = excluded.link,
        note = excluded.note,
        ttl = excluded.ttl,
        seq = excluded.seq,
        has_seq = excluded.has_seq,
        started_at = excluded.started_at,
        updated_at = excluded.updated_at,
        finished_at = excluded.finished_at,
        via = excluded.via,
        ip = excluded.ip,
        country = excluded.country,
        received_at = excluded.received_at`,
      job.agent,
      job.job_id,
      job.title,
      job.state,
      job.progress,
      job.link,
      job.note,
      job.ttl,
      job.seq,
      job.has_seq,
      job.started_at,
      job.updated_at,
      job.finished_at,
      job.via,
      job.ip,
      job.country,
      job.received_at,
    );
  }

  private allocCursor(): number {
    const rows = this.sql.exec<{ value: string }>(
      "SELECT value FROM meta WHERE key = 'next_cursor'",
    ).toArray();
    const current = rows.length > 0 ? Number(rows[0].value) : 0;
    const next = current + 1;
    this.sql.exec(
      `INSERT INTO meta (key, value) VALUES ('next_cursor', ?)
       ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
      String(next),
    );
    return next;
  }

  private pruneEvents(): void {
    const count = this.sql.exec<{ c: number }>("SELECT COUNT(*) AS c FROM events").one().c;
    if (count <= MAX_EVENTS) return;
    const excess = count - MAX_EVENTS;
    this.sql.exec(
      "DELETE FROM events WHERE cursor IN (SELECT cursor FROM events ORDER BY cursor ASC LIMIT ?)",
      excess,
    );
  }

  private jobViews(nowMs: number): Record<string, unknown>[] {
    const jobs = this.sql.exec<JobRow>(
      `SELECT ${JOB_COLUMNS} FROM jobs`,
    ).toArray();
    const views = jobs.map((job) => toJobJson(job, this.jobHistory(job.agent, job.job_id), nowMs));
    views.sort((a, b) => {
      const agentA = String(a.agent);
      const agentB = String(b.agent);
      if (agentA !== agentB) return agentA < agentB ? -1 : 1;
      const updatedA = String(a.updated_at);
      const updatedB = String(b.updated_at);
      if (updatedA !== updatedB) return updatedA < updatedB ? 1 : -1;
      const jobA = String(a.job_id);
      const jobB = String(b.job_id);
      if (jobA !== jobB) return jobA < jobB ? -1 : 1;
      return 0;
    });
    return views;
  }

  private jobHistory(agent: string, jobId: string): EventRow[] {
    const rows = this.sql.exec<EventRow>(
      `SELECT ${EVENT_COLUMNS} FROM (
        SELECT ${EVENT_COLUMNS} FROM events
        WHERE agent = ? AND job_id = ?
        ORDER BY cursor DESC LIMIT ?
      ) ORDER BY cursor ASC`,
      agent,
      jobId,
      MAX_JOB_HISTORY,
    ).toArray();
    return rows;
  }

  private recentEvents(limit: number): Record<string, unknown>[] {
    const rows = this.sql.exec<EventRow>(
      `SELECT ${EVENT_COLUMNS} FROM events ORDER BY cursor DESC LIMIT ?`,
      limit,
    ).toArray();
    return rows.map(toEventJson);
  }

  private publishJob(job: JobRow): void {
    const view = toJobJson(job, this.jobHistory(job.agent, job.job_id), Date.now());
    const frame = `event: job\ndata: ${JSON.stringify(view)}\n\n`;
    for (const sub of this.subs) sub.send(frame);
  }

  private newTokenId(): string {
    for (let attempt = 0; attempt < 5; attempt++) {
      const id = randomHex(8);
      const rows = this.sql.exec("SELECT 1 AS ok FROM tokens WHERE id = ?", id).toArray();
      if (rows.length === 0) return id;
    }
    throw new Error("token id space exhausted");
  }
}

function notCreated(): Response {
  return json(404, { ok: false, error: "workspace not found" });
}

function jobKey(agent: string, jobId: string): string {
  return `${agent}\0${jobId}`;
}

function tokenVia(token: TokenRow): string {
  if (token.label) return token.label;
  return token.id;
}

type MintInput = {
  kind: "read" | "write";
  agent: string | null;
  label: string | null;
};

function parseMint(body: string): MintInput | { error: string } {
  const trimmed = body.trim();
  if (trimmed === "") return { error: "empty body" };
  let data: unknown;
  try {
    data = JSON.parse(trimmed);
  } catch {
    return { error: "invalid json" };
  }
  if (typeof data !== "object" || data === null || Array.isArray(data)) {
    return { error: "invalid json" };
  }
  const record = data as Record<string, unknown>;
  if (record.kind !== "read" && record.kind !== "write") {
    return { error: "kind must be read or write" };
  }
  const kind = record.kind;
  let agent: string | null = null;
  const agentPresent = Object.prototype.hasOwnProperty.call(record, "agent") && record.agent !== null;
  if (kind === "read" && agentPresent) {
    return { error: "read tokens cannot set agent" };
  }
  if (kind === "write") {
    if (typeof record.agent !== "string") return { error: "agent is required" };
    const agentError = validateId(record.agent, "agent");
    if (agentError) return { error: agentError };
    agent = record.agent;
  }
  let label: string | null = null;
  if (Object.prototype.hasOwnProperty.call(record, "label") && record.label !== null) {
    if (typeof record.label !== "string") return { error: "label must be a string" };
    if (record.label.length > 200) return { error: "label is too long" };
    if (record.label !== "") label = record.label;
  }
  return { kind, agent, label };
}
