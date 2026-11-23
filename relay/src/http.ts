export const MAX_BODY_BYTES = 1 << 20;

export function json(
  status: number,
  body: unknown,
  extraHeaders?: Record<string, string>,
): Response {
  const headers = new Headers(extraHeaders);
  headers.set("Content-Type", "application/json; charset=utf-8");
  headers.set("X-Content-Type-Options", "nosniff");
  headers.set("Cache-Control", "no-store");
  return new Response(JSON.stringify(body), { status, headers });
}

export function text(status: number, body: string, contentType: string): Response {
  return new Response(body, {
    status,
    headers: {
      "Content-Type": contentType,
      "X-Content-Type-Options": "nosniff",
      "Cache-Control": "no-store",
    },
  });
}

export function methodNotAllowed(allow: string): Response {
  return json(405, { ok: false, error: "method not allowed" }, { Allow: allow });
}

export function decodePathSegment(raw: string, pattern: RegExp): string | null {
  let value: string;
  try {
    value = decodeURIComponent(raw);
  } catch {
    return null;
  }
  if (!pattern.test(value)) return null;
  return value;
}

export const WORKSPACE_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
export const TOKEN_ID = /^[A-Za-z0-9_-]{1,64}$/;

export async function readLimitedText(
  request: Request,
): Promise<{ ok: true; text: string } | { ok: false; response: Response }> {
  const declared = request.headers.get("content-length");
  if (declared !== null) {
    if (!/^\d+$/.test(declared)) {
      return { ok: false, response: json(400, { ok: false, error: "invalid content-length" }) };
    }
    if (Number(declared) > MAX_BODY_BYTES) {
      return { ok: false, response: json(400, { ok: false, error: "body too large" }) };
    }
  }
  const bytes = await readBody(request, MAX_BODY_BYTES);
  if (bytes === "too-large") {
    return { ok: false, response: json(400, { ok: false, error: "body too large" }) };
  }
  if (bytes === "unreadable") {
    return { ok: false, response: json(400, { ok: false, error: "unreadable body" }) };
  }
  return { ok: true, text: new TextDecoder().decode(bytes) };
}

async function readBody(
  request: Request,
  max: number,
): Promise<Uint8Array | "too-large" | "unreadable"> {
  const reader = request.body?.getReader();
  if (!reader) return new Uint8Array();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      if (!value) continue;
      total += value.byteLength;
      if (total > max) {
        await reader.cancel();
        return "too-large";
      }
      chunks.push(value);
    }
  } catch {
    return "unreadable";
  }
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return out;
}
