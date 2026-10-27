/**
 * Session probe and sign-out for the marketing site.
 *
 * The static export is served same-origin with the gateway (`/api/*` goes
 * through the edge), so the header can ask `GET /api/me` directly. Every
 * failure — 401, network, malformed body — means "guest": a marketing page
 * must render without a session and never surface a probe error in the chrome.
 */

const ME_PATH = "/api/me";
const LOGOUT_PATH = "/api/logout";

export interface HeaderSession {
  signedIn: boolean;
  /** `display_name` when the account has one, else `username`. */
  name?: string;
}

function field(body: Record<string, unknown>, key: string): string {
  const value = body[key];
  return typeof value === "string" ? value.trim() : "";
}

/** Resolves the header's session. Never rejects: a guest is the safe answer. */
export async function fetchHeaderSession(signal?: AbortSignal): Promise<HeaderSession> {
  try {
    const response = await fetch(ME_PATH, {
      credentials: "same-origin",
      cache: "no-store",
      headers: { Accept: "application/json" },
      signal,
    });
    if (!response.ok) return { signedIn: false };

    const body: unknown = await response.json();
    if (typeof body !== "object" || body === null) return { signedIn: false };

    const record = body as Record<string, unknown>;
    const name = field(record, "display_name") || field(record, "username");
    return name ? { signedIn: true, name } : { signedIn: false };
  } catch {
    return { signedIn: false };
  }
}

/**
 * Revokes the cookie session, then lands on the locale home page. Logout is
 * public-safe (no CSRF header) and a failed revoke must not trap the visitor
 * on a signed-in header — the landing page re-probes and shows the truth.
 */
export async function signOut(homePath: string): Promise<void> {
  try {
    await fetch(LOGOUT_PATH, {
      method: "POST",
      credentials: "same-origin",
      keepalive: true,
      headers: { Accept: "application/json" },
    });
  } catch {
    // Navigation below still runs; the next page decides the real state.
  } finally {
    window.location.assign(homePath);
  }
}
