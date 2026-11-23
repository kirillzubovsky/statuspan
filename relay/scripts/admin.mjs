#!/usr/bin/env node
// Admin CLI for the StatusPan relay. No dependencies.
//
//   node scripts/admin.mjs create-workspace <workspace>
//   node scripts/admin.mjs mint <workspace> --kind read|write [--agent <name>] [--label <label>] [--out <path>]
//   node scripts/admin.mjs list <workspace>
//   node scripts/admin.mjs revoke <workspace> <token-id>
//
// RELAY_URL and ADMIN_TOKEN come from the environment.
// --relay-url overrides RELAY_URL.
// --admin-token-file overrides ADMIN_TOKEN. The file is trimmed; it is never printed.
// mint --out writes only the token (mode 0600) and prints "saved to <path>".

import fs from "node:fs";

const USAGE = `Usage:
  admin.mjs create-workspace <workspace>
  admin.mjs mint <workspace> --kind read|write [--agent <name>] [--label <label>] [--out <path>]
  admin.mjs list <workspace>
  admin.mjs revoke <workspace> <token-id>

RELAY_URL and ADMIN_TOKEN from the environment, or --relay-url / --admin-token-file.`;

async function main() {
  const { positional, flags } = parseArgs(process.argv.slice(2));
  const command = positional[0];
  if (!command || flags.help === true) {
    console.log(USAGE);
    process.exitCode = command ? 0 : 1;
    return;
  }
  const relay = String(flags["relay-url"] ?? process.env.RELAY_URL ?? "").replace(/\/$/, "");
  if (!relay) {
    fail("RELAY_URL is required");
    return;
  }
  let adminToken = "";
  try {
    adminToken = readAdminToken(flags);
  } catch (err) {
    fail(err instanceof Error ? err.message : "failed to read admin token");
    return;
  }
  if (command === "create-workspace") {
    const workspace = needOne(positional, "workspace");
    if (!workspace) return;
    const res = await call(relay, adminToken, "POST", `/v1/admin/workspaces/${encodeURIComponent(workspace)}`);
    if (!res.ok) return;
    console.log(`created ${workspace}`);
    return;
  }
  if (command === "mint") {
    const workspace = needOne(positional, "workspace");
    if (!workspace) return;
    const kind = flags.kind;
    if (kind !== "read" && kind !== "write") {
      fail("--kind must be read or write");
      return;
    }
    const body = { kind };
    if (typeof flags.agent === "string") body.agent = flags.agent;
    if (typeof flags.label === "string") body.label = flags.label;
    const res = await call(
      relay,
      adminToken,
      "POST",
      `/v1/admin/workspaces/${encodeURIComponent(workspace)}/tokens`,
      body,
    );
    if (!res.ok) return;
    const token = res.data && typeof res.data.token === "string" ? res.data.token : "";
    const id = res.data && typeof res.data.id === "string" ? res.data.id : "";
    if (!token || !id) {
      fail("mint response missing token");
      return;
    }
    if (typeof flags.out === "string") {
      writeTokenFile(flags.out, token);
      console.log(`saved to ${flags.out}`);
      return;
    }
    console.log(JSON.stringify({ id, token, kind: res.data.kind, agent: res.data.agent ?? null, label: res.data.label ?? null }));
    return;
  }
  if (command === "list") {
    const workspace = needOne(positional, "workspace");
    if (!workspace) return;
    const res = await call(
      relay,
      adminToken,
      "GET",
      `/v1/admin/workspaces/${encodeURIComponent(workspace)}/tokens`,
    );
    if (!res.ok) return;
    console.log(JSON.stringify(res.data));
    return;
  }
  if (command === "revoke") {
    const workspace = positional[1];
    const id = positional[2];
    if (!workspace || !id) {
      fail("revoke requires <workspace> <token-id>");
      return;
    }
    const res = await call(
      relay,
      adminToken,
      "DELETE",
      `/v1/admin/workspaces/${encodeURIComponent(workspace)}/tokens/${encodeURIComponent(id)}`,
    );
    if (!res.ok) return;
    console.log(`revoked ${id}`);
    return;
  }
  fail(`unknown command ${command}`);
}

function needOne(positional, label) {
  const value = positional[1];
  if (!value) {
    fail(`${label} is required`);
    return "";
  }
  return value;
}

function readAdminToken(flags) {
  if (typeof flags["admin-token-file"] === "string") {
    const text = fs.readFileSync(flags["admin-token-file"], "utf8").trim();
    if (!text) throw new Error("admin token file is empty");
    return text;
  }
  const fromEnv = process.env.ADMIN_TOKEN ?? "";
  if (!fromEnv) throw new Error("ADMIN_TOKEN is required");
  return fromEnv;
}

function writeTokenFile(outPath, token) {
  const fh = fs.openSync(outPath, "w", 0o600);
  try {
    fs.fchmodSync(fh, 0o600);
    fs.writeFileSync(fh, `${token}\n`);
  } finally {
    fs.closeSync(fh);
  }
}

async function call(relay, adminToken, method, path, body) {
  const headers = { Authorization: `Bearer ${adminToken}` };
  if (body) headers["Content-Type"] = "application/json";
  let res;
  try {
    res = await fetch(`${relay}${path}`, {
      method,
      headers,
      body: body ? JSON.stringify(body) : undefined,
      signal: AbortSignal.timeout(30_000),
    });
  } catch (err) {
    fail(err instanceof Error ? err.message : "request failed");
    return { ok: false, data: null };
  }
  const text = await res.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (!res.ok) {
    const message = data && typeof data.error === "string" ? data.error : `http ${res.status}`;
    fail(message);
    return { ok: false, data };
  }
  return { ok: true, data };
}

function parseArgs(argv) {
  const positional = [];
  const flags = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--") {
      positional.push(...argv.slice(i + 1));
      break;
    }
    if (!arg.startsWith("--")) {
      positional.push(arg);
      continue;
    }
    const eq = arg.indexOf("=");
    if (eq !== -1) {
      flags[arg.slice(2, eq)] = arg.slice(eq + 1);
      continue;
    }
    const key = arg.slice(2);
    const next = argv[i + 1];
    if (next === undefined || next.startsWith("--")) {
      flags[key] = true;
      continue;
    }
    flags[key] = next;
    i++;
  }
  return { positional, flags };
}

function fail(message) {
  console.error(message);
  process.exitCode = 1;
}

main().catch((err) => {
  console.error(err instanceof Error ? err.message : "failed");
  process.exitCode = 1;
});
