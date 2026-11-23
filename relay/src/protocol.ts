// Job updates follow internal/store in the Go daemon: the same field names,
// the same states, and the same seq / partial-update rules.

export const DEFAULT_TTL = 600;
// Read-time only. A running job is abandoned after max(24h, 4*ttl), and only
// once it is already stalled. Queued and blocked use 7 days. Zero would disable
// a category; these defaults stay on. Abandoned is not a stored state.
export const DEFAULT_ABANDON_AFTER_MS = 24 * 60 * 60 * 1000;
export const DEFAULT_ABANDON_WAITING_AFTER_MS = 7 * 24 * 60 * 60 * 1000;
const ABANDON_TTL_FACTOR = 4;
export const MAX_JOB_HISTORY = 20;
export const MAX_EVENTS = 50_000;
export const DEFAULT_EVENT_LIMIT = 200;
export const MAX_EVENT_LIMIT = 1000;
export const MAX_BATCH = 1000;

const VALID_STATES = new Set(["queued", "running", "blocked", "done", "failed"]);

export type ParsedUpdate = {
  agent: string;
  jobId: string;
  state: string;
  title?: string;
  progress?: number;
  link?: string;
  note?: string;
  seq?: number;
  ttl?: number;
};

export type JobRow = {
  agent: string;
  job_id: string;
  title: string;
  state: string;
  progress: number;
  link: string;
  note: string;
  ttl: number;
  seq: number | null;
  has_seq: number;
  started_at: string;
  updated_at: string;
  finished_at: string | null;
};

export type EventRow = {
  cursor: number;
  agent: string;
  job_id: string;
  state: string;
  has_progress: number;
  progress: number | null;
  note: string;
  title: string;
  link: string;
  ts: string;
  applied: number;
  has_seq: number;
  seq: number | null;
  kind: string;
};

export function parseUpdates(text: string): { updates: ParsedUpdate[] } | { error: string } {
  const trimmed = text.trim();
  if (trimmed === "" || trimmed === "null") return { error: "empty body" };
  let data: unknown;
  try {
    data = JSON.parse(trimmed);
  } catch {
    return { error: "invalid json" };
  }
  if (Array.isArray(data)) {
    if (data.length > MAX_BATCH) return { error: "too many updates" };
    const updates: ParsedUpdate[] = [];
    for (const item of data) {
      const parsed = parseOne(item);
      if ("error" in parsed) return parsed;
      updates.push(parsed.update);
    }
    return { updates };
  }
  const parsed = parseOne(data);
  if ("error" in parsed) return parsed;
  return { updates: [parsed.update] };
}

function parseOne(value: unknown): { update: ParsedUpdate } | { error: string } {
  if (!isRecord(value)) return { error: "invalid json" };
  const agent = requiredString(value, "agent");
  const jobId = requiredString(value, "job_id");
  const state = readState(value);
  const title = optionalString(value, "title");
  const link = optionalString(value, "link");
  const note = optionalString(value, "note");
  const progress = optionalInt(value, "progress");
  const seq = optionalInt(value, "seq");
  const ttl = optionalInt(value, "ttl");
  if (
    !agent.ok || !jobId.ok || !state.ok || !title.ok || !link.ok ||
    !note.ok || !progress.ok || !seq.ok || !ttl.ok
  ) {
    return { error: "invalid json" };
  }
  const update: ParsedUpdate = {
    agent: agent.value,
    jobId: jobId.value,
    state: state.value,
  };
  if (title.value !== undefined) update.title = title.value;
  if (link.value !== undefined) update.link = link.value;
  if (note.value !== undefined) update.note = note.value;
  if (progress.value !== undefined) update.progress = progress.value;
  if (seq.value !== undefined) update.seq = seq.value;
  if (ttl.value !== undefined) update.ttl = ttl.value;
  return { update };
}

export function validateUpdate(u: ParsedUpdate): string | null {
  const agentError = validateId(u.agent, "agent");
  if (agentError) return agentError;
  const jobError = validateId(u.jobId, "job_id");
  if (jobError) return jobError;
  if (u.state !== "" && !VALID_STATES.has(u.state)) {
    return "state must be queued, running, blocked, done, or failed";
  }
  if (u.progress !== undefined && (u.progress < 0 || u.progress > 100)) {
    return "progress must be an integer from 0 to 100";
  }
  if (u.ttl !== undefined && u.ttl < 0) {
    return "ttl must be >= 0";
  }
  return null;
}

export function validateId(value: string, field: string): string | null {
  if (value.trim() === "") return `${field} is required`;
  if (/[/\\\u0000]/.test(value)) return `${field} must not contain / or \\`;
  return null;
}

export function stateRequiredMessage(): string {
  return "state must be queued, running, blocked, done, or failed";
}

// A missing seq is newer. An explicit seq applies only when the job has no
// seq yet or the new seq is strictly greater. Equal and older seq are history.
export function seqApplies(existing: JobRow | null, seq: number | undefined): boolean {
  if (!existing) return true;
  if (seq === undefined || existing.has_seq === 0) return true;
  return seq > (existing.seq ?? 0);
}

export function mergeJob(job: JobRow, u: ParsedUpdate, ts: string): void {
  if (u.title !== undefined) job.title = u.title;
  if (u.link !== undefined) job.link = u.link;
  if (u.progress !== undefined) job.progress = u.progress;
  if (u.note !== undefined) job.note = u.note;
  if (u.ttl !== undefined) job.ttl = u.ttl;
  if (u.state !== "") {
    job.state = u.state;
    if (u.state === "done" || u.state === "failed") {
      if (job.finished_at === null) job.finished_at = ts;
    } else {
      job.finished_at = null;
    }
  }
  if (u.seq !== undefined) {
    job.seq = u.seq;
    job.has_seq = 1;
  }
  job.updated_at = ts;
}

export function freshJob(u: ParsedUpdate, ts: string): JobRow {
  return {
    agent: u.agent,
    job_id: u.jobId,
    title: "",
    state: "",
    progress: 0,
    link: "",
    note: "",
    ttl: DEFAULT_TTL,
    seq: null,
    has_seq: 0,
    started_at: ts,
    updated_at: ts,
    finished_at: null,
  };
}

export function toEventJson(row: EventRow): Record<string, unknown> {
  const out: Record<string, unknown> = {
    cursor: row.cursor,
    n: row.cursor,
    agent: row.agent,
    job_id: row.job_id,
    ts: row.ts,
    applied: row.applied === 1,
    kind: row.kind,
  };
  if (row.state !== "") out.state = row.state;
  if (row.has_progress === 1) out.progress = row.progress;
  if (row.note !== "") out.note = row.note;
  if (row.title !== "") out.title = row.title;
  if (row.link !== "") out.link = row.link;
  if (row.has_seq === 1) out.seq = row.seq;
  return out;
}

export function toJobJson(job: JobRow, history: EventRow[], nowMs: number): Record<string, unknown> {
  const out: Record<string, unknown> = {
    agent: job.agent,
    job_id: job.job_id,
    title: job.title,
    state: job.state,
    progress: job.progress,
    ttl: job.ttl,
    started_at: job.started_at,
    updated_at: job.updated_at,
    elapsed_seconds: elapsedSeconds(job, nowMs),
    stalled: stalled(job, nowMs),
    abandoned: abandoned(job, nowMs),
    history: history.map(toEventJson),
  };
  const abandonedAtIso = abandonedAt(job, nowMs);
  if (abandonedAtIso) out.abandoned_at = abandonedAtIso;
  if (job.link !== "") out.link = job.link;
  if (job.note !== "") out.note = job.note;
  if (job.has_seq === 1) out.seq = job.seq;
  if (job.finished_at !== null) out.finished_at = job.finished_at;
  return out;
}

export function elapsedSeconds(job: JobRow, nowMs: number): number {
  let end = nowMs;
  if (job.finished_at && (job.state === "done" || job.state === "failed")) {
    end = Date.parse(job.finished_at);
  }
  const sec = Math.floor((end - Date.parse(job.started_at)) / 1000);
  return sec < 0 ? 0 : sec;
}

export function abandonLimitMs(job: JobRow): number | null {
  if (job.state === "running") {
    if (DEFAULT_ABANDON_AFTER_MS <= 0) return null;
    const ttlMs = job.ttl > 0 ? job.ttl * 1000 : 0;
    if (!Number.isFinite(ttlMs)) return null;
    const four = ttlMs * ABANDON_TTL_FACTOR;
    if (!Number.isFinite(four)) return null;
    return Math.max(DEFAULT_ABANDON_AFTER_MS, four);
  }
  if (job.state === "queued" || job.state === "blocked") {
    if (DEFAULT_ABANDON_WAITING_AFTER_MS <= 0) return null;
    return DEFAULT_ABANDON_WAITING_AFTER_MS;
  }
  return null;
}

export function abandoned(job: JobRow, nowMs: number): boolean {
  const limit = abandonLimitMs(job);
  if (limit == null || limit <= 0) return false;
  const updated = Date.parse(job.updated_at);
  if (Number.isNaN(updated)) return false;
  return nowMs - updated > limit;
}

// abandonedAt is updated_at plus the threshold that was crossed.
export function abandonedAt(job: JobRow, nowMs: number): string | null {
  if (!abandoned(job, nowMs)) return null;
  const limit = abandonLimitMs(job);
  if (limit == null) return null;
  const updated = Date.parse(job.updated_at);
  if (Number.isNaN(updated)) return null;
  return new Date(updated + limit).toISOString();
}

export function stalled(job: JobRow, nowMs: number): boolean {
  // Queued has not started. Blocked waits on a person. Only running goes stale.
  // Abandoned jobs are past stalled and do not count as stalled.
  if (job.state !== "running") return false;
  if (abandoned(job, nowMs)) return false;
  const age = nowMs - Date.parse(job.updated_at);
  if (age <= 0) return false;
  if (job.ttl <= 0) return true;
  return age > job.ttl * 1000;
}

export function parseSince(raw: string | null): number | "invalid" {
  if (raw === null || raw === "") return 0;
  if (!/^\d+$/.test(raw)) return "invalid";
  const n = Number(raw);
  if (!Number.isSafeInteger(n)) return "invalid";
  return n;
}

export function parseLimit(raw: string | null): number | "invalid" {
  if (raw === null || raw === "") return DEFAULT_EVENT_LIMIT;
  if (!/^\d+$/.test(raw)) return "invalid";
  const n = Number(raw);
  if (!Number.isSafeInteger(n)) return "invalid";
  if (n > MAX_EVENT_LIMIT) return MAX_EVENT_LIMIT;
  return n;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function requiredString(
  obj: Record<string, unknown>,
  key: string,
): { ok: true; value: string } | { ok: false } {
  if (!Object.prototype.hasOwnProperty.call(obj, key) || obj[key] === null) {
    return { ok: true, value: "" };
  }
  if (typeof obj[key] !== "string") return { ok: false };
  return { ok: true, value: obj[key] };
}

function readState(obj: Record<string, unknown>): { ok: true; value: string } | { ok: false } {
  if (!Object.prototype.hasOwnProperty.call(obj, "state")) return { ok: true, value: "" };
  if (typeof obj.state !== "string") return { ok: false };
  return { ok: true, value: obj.state };
}

function optionalString(
  obj: Record<string, unknown>,
  key: string,
): { ok: true; value?: string } | { ok: false } {
  if (!Object.prototype.hasOwnProperty.call(obj, key) || obj[key] === null) {
    return { ok: true };
  }
  if (typeof obj[key] !== "string") return { ok: false };
  return { ok: true, value: obj[key] };
}

function optionalInt(
  obj: Record<string, unknown>,
  key: string,
): { ok: true; value?: number } | { ok: false } {
  if (!Object.prototype.hasOwnProperty.call(obj, key) || obj[key] === null) {
    return { ok: true };
  }
  const value = obj[key];
  if (typeof value !== "number" || !Number.isSafeInteger(value)) return { ok: false };
  return { ok: true, value };
}
