import { env, runInDurableObject, SELF } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { sha256Hex } from "../src/auth";
import { Workspace, requestProvenance } from "../src/index";

const ADMIN = "test-admin-secret";
const BASE = "http://relay.test";

type Job = {
  agent: string;
  job_id: string;
  title: string;
  state: string;
  progress: number;
  link?: string;
  note?: string;
  seq?: number;
  ttl: number;
  history: Array<{
    cursor: number;
    applied: boolean;
    seq?: number;
    state?: string;
    note?: string;
    via?: string;
    ip?: string;
    country?: string;
    received_at?: string;
  }>;
  via?: string;
  ip?: string;
  country?: string;
  received_at?: string;
};

type ListedEvent = {
  cursor: number;
  n: number;
  agent: string;
  job_id: string;
  applied: boolean;
  seq?: number;
  state?: string;
  progress?: number;
  note?: string;
  title?: string;
  via?: string;
  ip?: string;
  country?: string;
  received_at?: string;
};

function workspaceName(): string {
  return `w${crypto.randomUUID().replaceAll("-", "").slice(0, 16)}`;
}

function adminHeaders(): Headers {
  return new Headers({ Authorization: `Bearer ${ADMIN}` });
}

async function bodyOf(res: Response): Promise<Record<string, unknown>> {
  return res.json() as Promise<Record<string, unknown>>;
}

async function createWorkspace(name: string): Promise<void> {
  const res = await SELF.fetch(`${BASE}/v1/admin/workspaces/${name}`, {
    method: "POST",
    headers: adminHeaders(),
  });
  expect(res.status).toBe(200);
}

async function mint(
  name: string,
  input: { kind: "read" | "write"; agent?: string; label?: string },
): Promise<{ id: string; token: string; kind: string; agent: string | null }> {
  const res = await SELF.fetch(`${BASE}/v1/admin/workspaces/${name}/tokens`, {
    method: "POST",
    headers: adminHeaders(),
    body: JSON.stringify(input),
  });
  expect(res.status).toBe(200);
  const data = await bodyOf(res);
  expect(typeof data.token).toBe("string");
  expect(typeof data.id).toBe("string");
  return data as { id: string; token: string; kind: string; agent: string | null };
}

function auth(token: string, init: RequestInit = {}): RequestInit {
  const headers = new Headers(init.headers);
  headers.set("Authorization", `Bearer ${token}`);
  return { ...init, headers };
}

async function postJobs(
  name: string,
  token: string,
  payload: unknown,
  headers?: HeadersInit,
): Promise<Response> {
  return SELF.fetch(`${BASE}/v1/w/${name}/jobs`, auth(token, {
    method: "POST",
    headers,
    body: JSON.stringify(payload),
  }));
}

async function getJobs(name: string, token: string): Promise<{ jobs: Job[] }> {
  const res = await SELF.fetch(`${BASE}/v1/w/${name}/jobs`, auth(token));
  expect(res.status).toBe(200);
  return res.json() as Promise<{ jobs: Job[] }>;
}

async function getEvents(name: string, token: string, query = ""): Promise<{
  events: ListedEvent[];
  next_cursor: number;
}> {
  const res = await SELF.fetch(`${BASE}/v1/w/${name}/events${query}`, auth(token));
  expect(res.status).toBe(200);
  return res.json() as Promise<{ events: ListedEvent[]; next_cursor: number }>;
}

describe("relay", () => {
  it("serves healthz without auth", async () => {
    const res = await SELF.fetch(`${BASE}/healthz`);
    expect(res.status).toBe(200);
    expect(await res.text()).toBe("ok\n");
  });

  it("requires admin auth", async () => {
    const name = workspaceName();
    const missing = await SELF.fetch(`${BASE}/v1/admin/workspaces/${name}`, { method: "POST" });
    expect(missing.status).toBe(401);
    const wrong = await SELF.fetch(`${BASE}/v1/admin/workspaces/${name}`, {
      method: "POST",
      headers: { Authorization: "Bearer not-the-admin" },
    });
    expect(wrong.status).toBe(401);
    const list = await SELF.fetch(`${BASE}/v1/admin/workspaces/${name}/tokens`);
    expect(list.status).toBe(401);
    const revoke = await SELF.fetch(`${BASE}/v1/admin/workspaces/${name}/tokens/abcdef0123456789`, {
      method: "DELETE",
    });
    expect(revoke.status).toBe(401);

    await createWorkspace(name);
    const minted = await mint(name, { kind: "read" });
    const listed = await SELF.fetch(`${BASE}/v1/admin/workspaces/${name}/tokens`, {
      headers: { Authorization: "Bearer wrong" },
    });
    expect(listed.status).toBe(401);
    const removed = await SELF.fetch(`${BASE}/v1/admin/workspaces/${name}/tokens/${minted.id}`, {
      method: "DELETE",
      headers: { Authorization: "Bearer wrong" },
    });
    expect(removed.status).toBe(401);
  });

  it("returns 404 for a workspace that was never created", async () => {
    const name = workspaceName();
    const read = await SELF.fetch(`${BASE}/v1/w/${name}/jobs`, auth("spr_nope"));
    expect(read.status).toBe(404);
    const write = await postJobs(name, "spw_nope", {
      agent: "alpha",
      job_id: "j",
      state: "running",
    });
    expect(write.status).toBe(404);
  });

  it("scopes write tokens to one agent and refuses reads", async () => {
    const name = workspaceName();
    await createWorkspace(name);
    const writer = await mint(name, { kind: "write", agent: "alpha", label: "laptop" });
    expect(writer.token.startsWith("spw_")).toBe(true);
    expect(writer.agent).toBe("alpha");

    const other = await postJobs(name, writer.token, {
      agent: "beta",
      job_id: "j",
      state: "running",
    });
    expect(other.status).toBe(403);

    const jobs = await SELF.fetch(`${BASE}/v1/w/${name}/jobs`, auth(writer.token));
    expect(jobs.status).toBe(403);
    const events = await SELF.fetch(`${BASE}/v1/w/${name}/events`, auth(writer.token));
    expect(events.status).toBe(403);

    const ok = await postJobs(name, writer.token, {
      agent: "alpha",
      job_id: "j",
      state: "queued",
      title: "One",
    });
    expect(ok.status).toBe(200);
    const payload = await bodyOf(ok);
    expect(payload.applied).toBe(true);

    const reader = await mint(name, { kind: "read" });
    const listed = await getJobs(name, reader.token);
    expect(listed.jobs.map((job) => job.job_id)).toEqual(["j"]);
    expect(listed.jobs.some((job) => job.agent === "beta")).toBe(false);
  });

  it("refuses writes from a read token and unknown tokens", async () => {
    const name = workspaceName();
    await createWorkspace(name);
    const reader = await mint(name, { kind: "read", label: "board" });
    expect(reader.token.startsWith("spr_")).toBe(true);

    const denied = await postJobs(name, reader.token, {
      agent: "alpha",
      job_id: "j",
      state: "running",
    });
    expect(denied.status).toBe(403);

    const missing = await SELF.fetch(`${BASE}/v1/w/${name}/jobs`);
    expect(missing.status).toBe(401);
    const bad = await SELF.fetch(`${BASE}/v1/w/${name}/jobs`, auth("spr_not-a-real-token"));
    expect(bad.status).toBe(401);
    const badPost = await postJobs(name, "spw_not-a-real-token", {
      agent: "alpha",
      job_id: "j",
      state: "running",
    });
    expect(badPost.status).toBe(401);

    const writer = await mint(name, { kind: "write", agent: "alpha" });
    const created = await postJobs(name, writer.token, {
      agent: "alpha",
      job_id: "j",
      state: "running",
    });
    expect(created.status).toBe(200);
    const jobs = await getJobs(name, reader.token);
    expect(jobs.jobs).toHaveLength(1);
    expect(jobs.jobs[0].state).toBe("running");
  });

  it("keeps the higher seq and records an older seq as applied false", async () => {
    const name = workspaceName();
    await createWorkspace(name);
    const writer = await mint(name, { kind: "write", agent: "my bot" });
    const reader = await mint(name, { kind: "read" });

    const created = await postJobs(name, writer.token, {
      agent: "my bot",
      job_id: "build 1",
      title: "Build v1",
      state: "running",
      progress: 40,
      link: "http://example.test/a",
      ttl: 600,
      seq: 5,
    });
    expect(created.status).toBe(200);
    expect((await bodyOf(created)).applied).toBe(true);

    const older = await postJobs(name, writer.token, {
      agent: "my bot",
      job_id: "build 1",
      title: "older",
      state: "failed",
      progress: 1,
      seq: 4,
    });
    expect(older.status).toBe(200);
    expect((await bodyOf(older)).applied).toBe(false);

    const jobs = await getJobs(name, reader.token);
    expect(jobs.jobs).toHaveLength(1);
    const job = jobs.jobs[0];
    expect(job.title).toBe("Build v1");
    expect(job.state).toBe("running");
    expect(job.progress).toBe(40);
    expect(job.link).toBe("http://example.test/a");
    expect(job.seq).toBe(5);
    expect(job.ttl).toBe(600);

    const feed = await getEvents(name, reader.token);
    const stale = feed.events.find((event) => event.seq === 4);
    const fresh = feed.events.find((event) => event.seq === 5);
    expect(stale?.applied).toBe(false);
    expect(stale?.title).toBe("older");
    expect(fresh?.applied).toBe(true);
    expect((stale?.cursor ?? 0) > (fresh?.cursor ?? 0)).toBe(true);
    expect(job.history.some((event) => event.seq === 4 && event.applied === false)).toBe(true);
  });

  it("keeps the previous state when a partial update omits it", async () => {
    const name = workspaceName();
    await createWorkspace(name);
    const writer = await mint(name, { kind: "write", agent: "my bot" });
    const reader = await mint(name, { kind: "read" });

    const tooSoon = await postJobs(name, writer.token, {
      agent: "my bot",
      job_id: "build 1",
      note: "too soon",
    });
    expect(tooSoon.status).toBe(400);
    expect((await getJobs(name, reader.token)).jobs).toHaveLength(0);

    const created = await postJobs(name, writer.token, {
      agent: "my bot",
      job_id: "build 1",
      state: "running",
      title: "Build v1",
      progress: 10,
      link: "http://example.test/a",
      seq: 4,
    });
    expect(created.status).toBe(200);

    const partial = await postJobs(name, writer.token, {
      agent: "my bot",
      job_id: "build 1",
      progress: 55,
      note: "halfway",
    });
    expect(partial.status).toBe(200);
    expect((await bodyOf(partial)).applied).toBe(true);

    const job = (await getJobs(name, reader.token)).jobs[0];
    expect(job.state).toBe("running");
    expect(job.title).toBe("Build v1");
    expect(job.progress).toBe(55);
    expect(job.note).toBe("halfway");
    expect(job.link).toBe("http://example.test/a");
    expect(job.seq).toBe(4);

    const badBatch = await postJobs(name, writer.token, [
      { agent: "my bot", job_id: "fresh", state: "queued" },
      { agent: "my bot", job_id: "nope", note: "x" },
    ]);
    expect(badBatch.status).toBe(400);
    const ids = (await getJobs(name, reader.token)).jobs.map((item) => item.job_id);
    expect(ids).toEqual(["build 1"]);

    const together = await postJobs(name, writer.token, [
      { agent: "my bot", job_id: "batch", state: "queued", title: "B" },
      { agent: "my bot", job_id: "batch", progress: 9 },
    ]);
    expect(together.status).toBe(200);
    const batch = (await getJobs(name, reader.token)).jobs.find((item) => item.job_id === "batch");
    expect(batch?.state).toBe("queued");
    expect(batch?.progress).toBe(9);
    expect(batch?.title).toBe("B");
  });

  it("returns only events after the since cursor and advances next_cursor", async () => {
    const name = workspaceName();
    await createWorkspace(name);
    const writer = await mint(name, { kind: "write", agent: "alpha" });
    const reader = await mint(name, { kind: "read" });

    await postJobs(name, writer.token, {
      agent: "alpha",
      job_id: "a",
      state: "queued",
      seq: 1,
    });
    const first = await getEvents(name, reader.token);
    expect(first.events).toHaveLength(1);
    const cursor = first.events[0].cursor;
    expect(first.events[0].n).toBe(cursor);
    expect(first.next_cursor).toBe(cursor);

    await postJobs(name, writer.token, {
      agent: "alpha",
      job_id: "a",
      state: "running",
      progress: 20,
      seq: 2,
    });
    const second = await getEvents(name, reader.token, `?since=${cursor}&limit=10`);
    expect(second.events).toHaveLength(1);
    expect(second.events[0].cursor).toBeGreaterThan(cursor);
    expect(second.events[0].seq).toBe(2);
    expect(second.events[0].state).toBe("running");
    expect(second.next_cursor).toBe(second.events[0].cursor);
    expect(second.next_cursor).toBeGreaterThan(first.next_cursor);

    const rest = await getEvents(name, reader.token, `?since=${second.next_cursor}`);
    expect(rest.events).toHaveLength(0);
    expect(rest.next_cursor).toBe(second.next_cursor);

    const page = await getEvents(name, reader.token, "?since=0&limit=1");
    expect(page.events).toHaveLength(1);
    expect(page.events[0].cursor).toBe(cursor);
    expect(page.next_cursor).toBe(cursor);
  });

  it("revokes a token and stores only its hash", async () => {
    const name = workspaceName();
    await createWorkspace(name);
    const writer = await mint(name, { kind: "write", agent: "alpha", label: "ci" });
    const reader = await mint(name, { kind: "read", label: "board" });
    const hash = await sha256Hex(reader.token);

    const stub = env.WORKSPACE.get(env.WORKSPACE.idFromName(name));
    await runInDurableObject(stub, async (_instance: Workspace, state) => {
      const rows = state.storage.sql.exec<{ id: string; hash: string; kind: string }>(
        "SELECT id, hash, kind FROM tokens",
      ).toArray();
      const stored = rows.find((row) => row.id === reader.id);
      expect(stored?.hash).toBe(hash);
      expect(stored?.kind).toBe("read");
      expect(JSON.stringify(rows)).not.toContain(reader.token);
      expect(JSON.stringify(rows)).not.toContain(writer.token);
    });

    const listRes = await SELF.fetch(`${BASE}/v1/admin/workspaces/${name}/tokens`, {
      headers: adminHeaders(),
    });
    expect(listRes.status).toBe(200);
    const listed = await listRes.text();
    expect(listed).not.toContain(reader.token);
    expect(listed).not.toContain(writer.token);
    expect(listed).not.toContain(hash);
    expect(listed).not.toContain("hash");
    const tokens = (JSON.parse(listed) as {
      tokens: Array<{ id: string; kind: string; agent: string | null; label: string | null; created: string }>;
    }).tokens;
    expect(tokens).toHaveLength(2);
    const byId = new Map(tokens.map((token) => [token.id, token]));
    expect(byId.get(writer.id)).toMatchObject({ kind: "write", agent: "alpha", label: "ci" });
    expect(byId.get(reader.id)).toMatchObject({ kind: "read", agent: null, label: "board" });
    expect(byId.get(writer.id)?.created).toEqual(expect.any(String));

    const before = await SELF.fetch(`${BASE}/v1/w/${name}/jobs`, auth(reader.token));
    expect(before.status).toBe(200);

    const revoked = await SELF.fetch(`${BASE}/v1/admin/workspaces/${name}/tokens/${reader.id}`, {
      method: "DELETE",
      headers: adminHeaders(),
    });
    expect(revoked.status).toBe(200);

    const after = await SELF.fetch(`${BASE}/v1/w/${name}/jobs`, auth(reader.token));
    expect(after.status).toBe(401);
    const stillWrites = await postJobs(name, writer.token, {
      agent: "alpha",
      job_id: "j",
      state: "blocked",
    });
    expect(stillWrites.status).toBe(200);

    const listAfter = await SELF.fetch(`${BASE}/v1/admin/workspaces/${name}/tokens`, {
      headers: adminHeaders(),
    });
    const remaining = (await bodyOf(listAfter)).tokens as Array<{ id: string }>;
    expect(remaining.map((token) => token.id)).toEqual([writer.id]);
  });

  it("rejects a body larger than 1 MiB", async () => {
    const name = workspaceName();
    await createWorkspace(name);
    const writer = await mint(name, { kind: "write", agent: "alpha" });
    const note = "x".repeat(1 << 20);
    const res = await postJobs(name, writer.token, {
      agent: "alpha",
      job_id: "j",
      state: "running",
      note,
    });
    expect(res.status).toBe(400);
    const reader = await mint(name, { kind: "read" });
    expect((await getJobs(name, reader.token)).jobs).toHaveLength(0);
  });

  it("opens an SSE snapshot for a read token", async () => {
    const name = workspaceName();
    await createWorkspace(name);
    const reader = await mint(name, { kind: "read" });
    const writer = await mint(name, { kind: "write", agent: "alpha" });
    const denied = await SELF.fetch(`${BASE}/v1/w/${name}/stream`, auth(writer.token));
    expect(denied.status).toBe(403);

    const res = await SELF.fetch(`${BASE}/v1/w/${name}/stream`, auth(reader.token));
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("text/event-stream");
    const readerBody = res.body?.getReader();
    expect(readerBody).toBeTruthy();
    const first = await readerBody!.read();
    const text = new TextDecoder().decode(first.value);
    expect(text).toContain("event: snapshot");
    expect(text).toContain("\"jobs\"");
    await readerBody!.cancel();
  });

  it("reads ip and country from the request, with cf.country ahead of the header", () => {
    const headersOnly = new Request("https://relay.test/v1/w/home/jobs", {
      headers: {
        "CF-Connecting-IP": " 203.0.113.10 ",
        "CF-IPCountry": "US",
      },
    });
    expect(requestProvenance(headersOnly)).toEqual({ ip: "203.0.113.10", country: "US" });

    const fromCf = new Request("https://relay.test/v1/w/home/jobs", {
      headers: {
        "CF-Connecting-IP": "203.0.113.10",
        "CF-IPCountry": "DE",
      },
      cf: { country: "US" },
    } as RequestInit);
    expect(requestProvenance(fromCf).country).toBe("US");
    expect(requestProvenance(new Request("https://relay.test/")).ip).toBe("");
    expect(requestProvenance(new Request("https://relay.test/")).country).toBe("");
  });

  it("stamps writes from the token and the request, not the body", async () => {
    const name = workspaceName();
    await createWorkspace(name);
    const writer = await mint(name, { kind: "write", agent: "ci", label: "ci-builder" });
    const reader = await mint(name, { kind: "read" });
    const spoofed = {
      agent: "ci",
      job_id: "build-1",
      state: "running",
      note: "on host.example",
      via: "evil-host",
      ip: "198.51.100.1",
      country: "ZZ",
      received_at: "2000-01-01T00:00:00.000Z",
    };
    const created = await postJobs(name, writer.token, spoofed, {
      "CF-Connecting-IP": "203.0.113.10",
      "CF-IPCountry": "US",
    });
    expect(created.status).toBe(200);
    expect((await bodyOf(created)).applied).toBe(true);

    const events = await getEvents(name, reader.token);
    expect(events.events).toHaveLength(1);
    const event = events.events[0];
    expect(event.via).toBe("ci-builder");
    expect(event.ip).toBe("203.0.113.10");
    expect(event.country).toBe("US");
    expect(event.received_at).not.toBe("2000-01-01T00:00:00.000Z");
    expect(Date.parse(event.received_at ?? "")).toBeGreaterThan(Date.now() - 60_000);
    expect(JSON.stringify(event)).not.toContain("evil-host");
    expect(JSON.stringify(event)).not.toContain("198.51.100.1");

    const jobs = await getJobs(name, reader.token);
    const job = jobs.jobs[0];
    expect(job.via).toBe("ci-builder");
    expect(job.ip).toBe("203.0.113.10");
    expect(job.country).toBe("US");
    expect(job.received_at).toBe(event.received_at);
    expect(job.history).toHaveLength(1);
    expect(job.history[0].via).toBe("ci-builder");
    expect(job.history[0].ip).toBe("203.0.113.10");
    expect(job.history[0].country).toBe("US");
    expect(job.history[0].received_at).toBe(event.received_at);

    const older = await postJobs(name, writer.token, {
      ...spoofed,
      seq: 1,
      state: "failed",
      via: "still-spoofed",
    }, {
      "CF-Connecting-IP": "203.0.113.10",
      "CF-IPCountry": "US",
    });
    expect(older.status).toBe(200);
    expect((await bodyOf(older)).applied).toBe(true);
    const again = await postJobs(name, writer.token, {
      agent: "ci",
      job_id: "build-1",
      seq: 1,
      state: "done",
      via: "still-spoofed",
      ip: "198.51.100.9",
      country: "ZZ",
      received_at: "1999-01-01T00:00:00.000Z",
    }, {
      "CF-Connecting-IP": "203.0.113.23",
      "CF-IPCountry": "CA",
    });
    expect(again.status).toBe(200);
    expect((await bodyOf(again)).applied).toBe(false);
    const after = await getJobs(name, reader.token);
    expect(after.jobs[0].via).toBe("ci-builder");
    expect(after.jobs[0].ip).toBe("203.0.113.10");
    expect(after.jobs[0].country).toBe("US");
    expect(after.jobs[0].state).toBe("failed");
    const page = await getEvents(name, reader.token);
    const rejected = page.events[page.events.length - 1];
    expect(rejected.applied).toBe(false);
    expect(rejected.via).toBe("ci-builder");
    expect(rejected.ip).toBe("203.0.113.23");
    expect(rejected.country).toBe("CA");
    expect(rejected.received_at).not.toBe("1999-01-01T00:00:00.000Z");
    expect(JSON.stringify(page)).not.toContain("still-spoofed");
  });

  it("keeps the token label when the request has no network headers", async () => {
    const name = workspaceName();
    await createWorkspace(name);
    const writer = await mint(name, { kind: "write", agent: "ci", label: "ci-builder" });
    const reader = await mint(name, { kind: "read" });
    const created = await postJobs(name, writer.token, {
      agent: "ci",
      job_id: "build-1",
      state: "queued",
      via: "evil-host",
      ip: "198.51.100.1",
      country: "ZZ",
      received_at: "2000-01-01T00:00:00.000Z",
    });
    expect(created.status).toBe(200);
    const events = await getEvents(name, reader.token);
    expect(events.events[0].via).toBe("ci-builder");
    expect(events.events[0].ip ?? "").toBe("");
    expect(events.events[0].country ?? "").toBe("");
    expect(events.events[0].received_at).not.toBe("2000-01-01T00:00:00.000Z");
    const job = (await getJobs(name, reader.token)).jobs[0];
    expect(job.via).toBe("ci-builder");
    expect(job.ip ?? "").toBe("");
    expect(job.country ?? "").toBe("");
    expect(JSON.stringify(job)).not.toContain("evil-host");
    expect(JSON.stringify(job)).not.toContain("198.51.100.1");
  });

  it("falls back to the token id when the write token has no label", async () => {
    const name = workspaceName();
    await createWorkspace(name);
    const writer = await mint(name, { kind: "write", agent: "ci" });
    const reader = await mint(name, { kind: "read" });
    const created = await postJobs(name, writer.token, {
      agent: "ci",
      job_id: "build-1",
      state: "running",
      via: "evil-host",
    });
    expect(created.status).toBe(200);
    const event = (await getEvents(name, reader.token)).events[0];
    expect(event.via).toBe(writer.id);
    expect(event.via).not.toBe("evil-host");
    expect(event.via).not.toBe("ci");
  });
});
