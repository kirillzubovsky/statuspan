// Cloud waitlist API. Only POST /api/waitlist writes a row. There is no read route.

const MAX_BODY_BYTES = 16 * 1024;
const ALLOWED_ORIGINS = new Set([
  "https://statuspan.com",
  "https://www.statuspan.com",
]);
const USER_COUNTS = new Set(["1", "2-5", "6-20", "21-100", "100+"]);
const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
const FIELDS = ["name", "email", "company", "users", "comments", "website"];

export default {
  async fetch(request, env) {
    try {
      const url = new URL(request.url);
      if (url.pathname !== "/api/waitlist") {
        return json({ ok: false, error: "Not found" }, 404);
      }
      return await handleWaitlist(request, env);
    } catch {
      return corsJson(request, { ok: false, error: "Something went wrong. Please try again." }, 500);
    }
  },
};

async function handleWaitlist(request, env) {
  if (request.method === "OPTIONS") {
    return applyCors(new Response(null, { status: 204 }), request);
  }
  if (request.method !== "POST") {
    return corsJson(request, { ok: false, error: "Method not allowed" }, 405, {
      Allow: "POST, OPTIONS",
    });
  }

  const body = await readBody(request);
  if (body.tooLarge) {
    return corsJson(request, { ok: false, error: "Request too large." }, 413);
  }
  if (body.invalid) {
    return corsJson(request, { ok: false, error: "Invalid request." }, 400);
  }

  const parsed = parseFields(request.headers.get("content-type") || "", body.text);
  if (parsed.error) {
    return corsJson(request, { ok: false, error: parsed.error }, 400);
  }

  const problem = validate(parsed.fields);
  if (problem) {
    return corsJson(request, { ok: false, error: problem }, 400);
  }

  const fields = parsed.fields;
  const inserted = await env.DB.prepare(
    `INSERT INTO waitlist (created_at, name, email, company, users, comments)
     VALUES (?, ?, ?, ?, ?, ?)
     ON CONFLICT(email) DO NOTHING`
  ).bind(
    new Date().toISOString(),
    fields.name,
    fields.email,
    fields.company,
    fields.users,
    fields.comments
  ).run();
  if (inserted && inserted.success === false) {
    return corsJson(request, { ok: false, error: "Something went wrong. Please try again." }, 500);
  }

  return corsJson(request, { ok: true }, 200);
}

async function readBody(request) {
  const declared = request.headers.get("content-length");
  if (declared !== null && declared !== "") {
    const size = Number(declared);
    if (Number.isFinite(size) && size > MAX_BODY_BYTES) return { tooLarge: true };
  }
  if (!request.body) return { text: "" };

  const reader = request.body.getReader();
  const chunks = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > MAX_BODY_BYTES) {
        await cancelReader(reader);
        return { tooLarge: true };
      }
      chunks.push(value);
    }
  } catch {
    await cancelReader(reader);
    return { invalid: true };
  }

  const bytes = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return { text: new TextDecoder().decode(bytes) };
}

async function cancelReader(reader) {
  try {
    await reader.cancel();
  } catch {
    // The caller already decided the response.
  }
}

function parseFields(contentType, text) {
  const type = contentType.split(";")[0].trim().toLowerCase();
  if (type === "application/json") return parseJson(text);
  if (type === "application/x-www-form-urlencoded") return parseForm(text);
  return { error: "Invalid request." };
}

function parseJson(text) {
  let data;
  try {
    data = JSON.parse(text);
  } catch {
    return { error: "Invalid request." };
  }
  if (!data || typeof data !== "object" || Array.isArray(data)) {
    return { error: "Invalid request." };
  }
  const raw = {};
  for (const key of FIELDS) {
    if (!Object.prototype.hasOwnProperty.call(data, key) || data[key] == null) {
      raw[key] = "";
      continue;
    }
    if (typeof data[key] !== "string") return { error: "Invalid request." };
    raw[key] = data[key];
  }
  return { fields: normalize(raw) };
}

function parseForm(text) {
  const params = new URLSearchParams(text);
  const raw = {};
  for (const key of FIELDS) raw[key] = params.get(key) ?? "";
  return { fields: normalize(raw) };
}

function normalize(raw) {
  return {
    name: raw.name.trim(),
    email: raw.email.trim().toLowerCase(),
    company: raw.company.trim(),
    users: raw.users.trim(),
    comments: raw.comments.trim(),
    website: raw.website.trim(),
  };
}

function validate(fields) {
  if (fields.website) return "Rejected.";
  if (!fields.name) return "Name is required.";
  if (fields.name.length > 120) return "Name must be 120 characters or fewer.";
  if (!fields.email) return "Email is required.";
  if (fields.email.length > 254) return "Email must be 254 characters or fewer.";
  if (fields.email.length < 3 || !EMAIL_RE.test(fields.email)) return "Enter a valid email address.";
  if (fields.company.length > 200) return "Company must be 200 characters or fewer.";
  if (fields.comments.length > 2000) return "Comments must be 2,000 characters or fewer.";
  if (fields.users && !USER_COUNTS.has(fields.users)) return "Choose a number of users from the list.";
  return "";
}

function json(payload, status, extra) {
  const headers = new Headers(extra);
  headers.set("Content-Type", "application/json; charset=utf-8");
  headers.set("Cache-Control", "no-store");
  return new Response(JSON.stringify(payload), { status, headers });
}

function corsJson(request, payload, status, extra) {
  return applyCors(json(payload, status, extra), request);
}

function applyCors(response, request) {
  const origin = request.headers.get("Origin");
  if (!origin || !ALLOWED_ORIGINS.has(origin)) return response;
  const headers = new Headers(response.headers);
  headers.set("Access-Control-Allow-Origin", origin);
  headers.set("Access-Control-Allow-Methods", "POST, OPTIONS");
  headers.set("Access-Control-Allow-Headers", "Content-Type");
  headers.set("Vary", "Origin");
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
}
