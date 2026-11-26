import { adminAuthorized, bearerToken, sha256Hex } from "./auth";
import {
  TOKEN_ID,
  WORKSPACE_NAME,
  decodePathSegment,
  json,
  methodNotAllowed,
  readLimitedText,
  text,
} from "./http";
import { parseLimit, parseSince } from "./protocol";
import { Workspace } from "./workspace";

export { Workspace };

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);
    if (url.pathname === "/healthz") {
      if (request.method !== "GET") return methodNotAllowed("GET");
      return text(200, "ok\n", "text/plain; charset=utf-8");
    }
    try {
      return await route(request, env, url);
    } catch (err) {
      console.error(err instanceof Error ? err.message : "request failed");
      return json(500, { ok: false, error: "internal error" });
    }
  },
} satisfies ExportedHandler<Env>;

async function route(request: Request, env: Env, url: URL): Promise<Response> {
  const path = url.pathname;
  const adminToken = path.match(/^\/v1\/admin\/workspaces\/([^/]+)\/tokens\/([^/]+)$/);
  if (adminToken) {
    if (request.method !== "DELETE") return methodNotAllowed("DELETE");
    if (!adminAuthorized(request, env)) return json(401, { ok: false, error: "unauthorized" });
    const workspace = decodePathSegment(adminToken[1], WORKSPACE_NAME);
    const id = decodePathSegment(adminToken[2], TOKEN_ID);
    if (!workspace || !id) return json(400, { ok: false, error: "invalid workspace" });
    return stub(env, workspace).adminRevoke(id);
  }

  const adminTokens = path.match(/^\/v1\/admin\/workspaces\/([^/]+)\/tokens$/);
  if (adminTokens) {
    const workspace = decodePathSegment(adminTokens[1], WORKSPACE_NAME);
    if (!workspace) return json(400, { ok: false, error: "invalid workspace" });
    if (!adminAuthorized(request, env)) return json(401, { ok: false, error: "unauthorized" });
    if (request.method === "GET") return stub(env, workspace).adminListTokens();
    if (request.method === "POST") {
      const body = await readLimitedText(request);
      if (!body.ok) return body.response;
      return stub(env, workspace).adminMint(body.text);
    }
    return methodNotAllowed("GET, POST");
  }

  const adminWorkspace = path.match(/^\/v1\/admin\/workspaces\/([^/]+)$/);
  if (adminWorkspace) {
    if (request.method !== "POST") return methodNotAllowed("POST");
    if (!adminAuthorized(request, env)) return json(401, { ok: false, error: "unauthorized" });
    const workspace = decodePathSegment(adminWorkspace[1], WORKSPACE_NAME);
    if (!workspace) return json(400, { ok: false, error: "invalid workspace" });
    return stub(env, workspace).adminCreate();
  }

  const jobs = path.match(/^\/v1\/w\/([^/]+)\/jobs$/);
  if (jobs) {
    const workspace = decodePathSegment(jobs[1], WORKSPACE_NAME);
    if (!workspace) return json(400, { ok: false, error: "invalid workspace" });
    const tokenHash = await hashBearer(request);
    if (request.method === "GET") return stub(env, workspace).listJobs(tokenHash);
    if (request.method === "POST") {
      const body = await readLimitedText(request);
      if (!body.ok) return body.response;
      return stub(env, workspace).postJobs(tokenHash, body.text, requestProvenance(request));
    }
    return methodNotAllowed("GET, POST");
  }

  const events = path.match(/^\/v1\/w\/([^/]+)\/events$/);
  if (events) {
    if (request.method !== "GET") return methodNotAllowed("GET");
    const workspace = decodePathSegment(events[1], WORKSPACE_NAME);
    if (!workspace) return json(400, { ok: false, error: "invalid workspace" });
    const since = parseSince(url.searchParams.get("since"));
    const limit = parseLimit(url.searchParams.get("limit"));
    const tokenHash = await hashBearer(request);
    return stub(env, workspace).listEvents(tokenHash, since, limit);
  }

  const stream = path.match(/^\/v1\/w\/([^/]+)\/stream$/);
  if (stream) {
    if (request.method !== "GET") return methodNotAllowed("GET");
    const workspace = decodePathSegment(stream[1], WORKSPACE_NAME);
    if (!workspace) return json(400, { ok: false, error: "invalid workspace" });
    const tokenHash = await hashBearer(request);
    return stub(env, workspace).stream(tokenHash);
  }

  return json(404, { ok: false, error: "not found" });
}

function stub(env: Env, workspace: string): DurableObjectStub<Workspace> {
  const id = env.WORKSPACE.idFromName(workspace);
  return env.WORKSPACE.get(id);
}

export type RequestNet = {
  ip: string;
  country: string;
};

// Server-side source of a write. The client cannot set these. cf.country wins
// when the edge populated it; otherwise the country header is the fallback.
export function requestProvenance(request: Request): RequestNet {
  const ip = headerTrim(request, "CF-Connecting-IP");
  const fromCf = cfCountry(request);
  const country = fromCf !== "" ? fromCf : headerTrim(request, "CF-IPCountry");
  return { ip, country };
}

function headerTrim(request: Request, name: string): string {
  return (request.headers.get(name) ?? "").trim();
}

function cfCountry(request: Request): string {
  const cf = request.cf;
  if (!cf || typeof cf !== "object") return "";
  const country = (cf as { country?: unknown }).country;
  if (typeof country !== "string") return "";
  return country.trim();
}

async function hashBearer(request: Request): Promise<string | null> {
  const token = bearerToken(request);
  if (!token) return null;
  return sha256Hex(token);
}
