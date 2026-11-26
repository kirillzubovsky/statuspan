import { describe, expect, it } from "vitest";
import {
  abandoned,
  abandonedAt,
  parseUpdates,
  stalled,
  toEventJson,
  toJobJson,
  validateUpdate,
  type EventRow,
  type JobRow,
} from "../src/protocol";

const UPDATED = "2026-11-22T00:00:11.000Z";
const UPDATED_MS = Date.parse(UPDATED);
const DAY = 24 * 60 * 60 * 1000;

function row(partial: Partial<JobRow> = {}): JobRow {
  return {
    agent: "bot",
    job_id: "run",
    title: "launch",
    state: "running",
    progress: 10,
    link: "",
    note: "",
    ttl: 7200,
    seq: 1,
    has_seq: 1,
    started_at: UPDATED,
    updated_at: UPDATED,
    finished_at: null,
    via: "",
    ip: "",
    country: "",
    received_at: "",
    ...partial,
  };
}

function eventRow(partial: Partial<EventRow> = {}): EventRow {
  return {
    cursor: 1,
    agent: "a",
    job_id: "j",
    state: "running",
    has_progress: 0,
    progress: null,
    note: "",
    title: "",
    link: "",
    ts: UPDATED,
    applied: 1,
    has_seq: 0,
    seq: null,
    kind: "update",
    via: "",
    ip: "",
    country: "",
    received_at: "",
    ...partial,
  };
}

describe("abandoned", () => {
  it("is stalled and not abandoned just after ttl", () => {
    const job = row({ ttl: 7200 });
    const now = UPDATED_MS + 7200 * 1000 + 1;
    expect(stalled(job, now)).toBe(true);
    expect(abandoned(job, now)).toBe(false);
    expect(toJobJson(job, [], now).abandoned_at).toBeUndefined();
  });

  it("uses 24h when 4*ttl is shorter, and clears stalled", () => {
    const job = row({ ttl: 7200 });
    const boundary = UPDATED_MS + DAY;
    expect(abandoned(job, boundary)).toBe(false);
    expect(stalled(job, boundary)).toBe(true);
    const after = boundary + 1;
    expect(abandoned(job, after)).toBe(true);
    expect(stalled(job, after)).toBe(false);
    const json = toJobJson(job, [], after);
    expect(json.state).toBe("running");
    expect(json.abandoned).toBe(true);
    expect(json.stalled).toBe(false);
    expect(json.abandoned_at).toBe(new Date(boundary).toISOString());
  });

  it("uses 4*ttl when that is longer than 24h", () => {
    const ttl = 7 * 3600;
    const job = row({ ttl });
    const boundary = UPDATED_MS + ttl * 4 * 1000;
    expect(abandoned(job, UPDATED_MS + DAY + 1)).toBe(false);
    expect(stalled(job, UPDATED_MS + DAY + 1)).toBe(true);
    expect(abandoned(job, boundary)).toBe(false);
    expect(abandoned(job, boundary + 1)).toBe(true);
    expect(stalled(job, boundary + 1)).toBe(false);
    expect(abandonedAt(job, boundary + 1)).toBe(new Date(boundary).toISOString());
  });

  it("abandons queued and blocked after 7d and never stalls them", () => {
    for (const state of ["queued", "blocked"] as const) {
      const job = row({ state, ttl: 10 });
      expect(abandoned(job, UPDATED_MS + DAY + 1)).toBe(false);
      expect(stalled(job, UPDATED_MS + 8 * DAY)).toBe(false);
      const boundary = UPDATED_MS + 7 * DAY;
      expect(abandoned(job, boundary)).toBe(false);
      expect(abandoned(job, boundary + 1)).toBe(true);
      expect(stalled(job, boundary + 1)).toBe(false);
    }
  });

  it("never abandons done or failed", () => {
    for (const state of ["done", "failed"] as const) {
      const job = row({ state, finished_at: UPDATED });
      const later = UPDATED_MS + 30 * DAY;
      expect(abandoned(job, later)).toBe(false);
      expect(stalled(job, later)).toBe(false);
      expect(toJobJson(job, [], later).abandoned).toBe(false);
    }
  });

  it("rejects state abandoned", () => {
    expect(validateUpdate({ agent: "a", jobId: "j", state: "abandoned" })).toMatch(/queued, running, blocked, done, or failed/);
  });
});

describe("provenance", () => {
  it("ignores client via, ip, country, and received_at", () => {
    const parsed = parseUpdates(JSON.stringify({
      agent: "a",
      job_id: "j",
      state: "running",
      via: "evil-host",
      ip: "198.51.100.1",
      country: "ZZ",
      received_at: "2000-01-01T00:00:00.000Z",
    }));
    expect(parsed).toEqual({ updates: [{ agent: "a", jobId: "j", state: "running" }] });
  });

  it("omits empty provenance and includes a stamp when set", () => {
    expect(toEventJson(eventRow())).not.toHaveProperty("via");
    expect(toEventJson(eventRow())).not.toHaveProperty("ip");
    const stamped = toEventJson(eventRow({
      via: "ci-builder",
      ip: "203.0.113.10",
      country: "US",
      received_at: "2026-11-26T12:00:00.000Z",
    }));
    expect(stamped.via).toBe("ci-builder");
    expect(stamped.ip).toBe("203.0.113.10");
    expect(stamped.country).toBe("US");
    expect(stamped.received_at).toBe("2026-11-26T12:00:00.000Z");

    const job = toJobJson(row({
      via: "ci-builder",
      ip: "203.0.113.10",
      country: "US",
      received_at: "2026-11-26T12:00:00.000Z",
    }), [], UPDATED_MS);
    expect(job.via).toBe("ci-builder");
    expect(job.ip).toBe("203.0.113.10");
    expect(job.country).toBe("US");
    expect(job.received_at).toBe("2026-11-26T12:00:00.000Z");
    expect(toJobJson(row(), [], UPDATED_MS).via).toBeUndefined();
  });
});
