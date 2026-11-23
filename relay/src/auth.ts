// Token material and the admin-secret compare. Tokens are random 32 bytes
// with a kind prefix. Only the SHA-256 hex of the full string is stored.

export async function sha256Hex(value: string): Promise<string> {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(value),
  );
  const bytes = new Uint8Array(digest);
  let hex = "";
  for (let i = 0; i < bytes.length; i++) {
    hex += bytes[i].toString(16).padStart(2, "0");
  }
  return hex;
}

export function randomHex(byteLength: number): string {
  const bytes = new Uint8Array(byteLength);
  crypto.getRandomValues(bytes);
  let hex = "";
  for (let i = 0; i < bytes.length; i++) {
    hex += bytes[i].toString(16).padStart(2, "0");
  }
  return hex;
}

export function randomToken(kind: "read" | "write"): string {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  const prefix = kind === "write" ? "spw_" : "spr_";
  return prefix + base64Url(bytes);
}

function base64Url(bytes: Uint8Array): string {
  let binary = "";
  for (let i = 0; i < bytes.length; i++) {
    binary += String.fromCharCode(bytes[i]);
  }
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

export function bearerToken(request: Request): string | null {
  const header = request.headers.get("Authorization");
  if (!header) return null;
  const match = /^Bearer\s+(\S+)\s*$/i.exec(header);
  return match ? match[1] : null;
}

// Length mismatches still touch timingSafeEqual so the reject path is not a
// single early return on the first differing byte of the secret.
export function timingSafeEqualString(left: string, right: string): boolean {
  const enc = new TextEncoder();
  const a = enc.encode(left);
  const b = enc.encode(right);
  if (a.byteLength !== b.byteLength) {
    crypto.subtle.timingSafeEqual(a, a);
    return false;
  }
  return crypto.subtle.timingSafeEqual(a, b);
}

export function adminAuthorized(request: Request, env: Env): boolean {
  const expected = env.ADMIN_TOKEN;
  if (!expected) return false;
  const got = bearerToken(request);
  if (!got) return false;
  return timingSafeEqualString(got, expected);
}
