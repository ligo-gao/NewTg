import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fetchHeaderSession, signOut } from "./session";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("fetchHeaderSession", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("reports the display name when the session has one", async () => {
    const fetchMock = vi.fn(async () =>
      jsonResponse({ username: "ada", display_name: "Ada Lovelace" }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await expect(fetchHeaderSession()).resolves.toEqual({
      signedIn: true,
      name: "Ada Lovelace",
    });
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/me",
      expect.objectContaining({ credentials: "same-origin" }),
    );
  });

  it("falls back to the username when display_name is empty", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse({ username: "ada", display_name: null })),
    );

    await expect(fetchHeaderSession()).resolves.toEqual({ signedIn: true, name: "ada" });
  });

  it("treats 401 as a guest without surfacing an error", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({}, 401)));

    await expect(fetchHeaderSession()).resolves.toEqual({ signedIn: false });
  });

  it("stays a guest when the probe itself fails", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("network down");
      }),
    );

    await expect(fetchHeaderSession()).resolves.toEqual({ signedIn: false });
  });
});

describe("signOut", () => {
  const originalLocation = window.location;
  let assign: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    assign = vi.fn();
    Object.defineProperty(window, "location", {
      writable: true,
      value: { ...originalLocation, assign },
    });
  });

  afterEach(() => {
    Object.defineProperty(window, "location", {
      writable: true,
      value: originalLocation,
    });
    vi.unstubAllGlobals();
  });

  it("revokes the session, then lands on the locale home page", async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);

    await signOut("/en/");

    expect(fetchMock).toHaveBeenCalledWith(
      "/api/logout",
      expect.objectContaining({ method: "POST", credentials: "same-origin" }),
    );
    expect(assign).toHaveBeenCalledWith("/en/");
  });

  it("still leaves the page when the revoke request fails", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("network down");
      }),
    );

    await signOut("/");

    expect(assign).toHaveBeenCalledWith("/");
  });
});
