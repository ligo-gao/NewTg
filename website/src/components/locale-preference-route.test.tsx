import { act, render, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { LocalePreferenceRoute } from "./locale-preference-route";

const router = vi.hoisted(() => ({ pathname: "/" }));

vi.mock("next/navigation", () => ({
  usePathname: () => router.pathname,
}));

const mockAssign = vi.fn();
let originalLocation: Location;

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("LocalePreferenceRoute", () => {
  beforeEach(() => {
    originalLocation = window.location;
    Object.defineProperty(window, "location", {
      configurable: true,
      writable: true,
      value: { ...originalLocation, assign: mockAssign, pathname: "/" },
    });
    router.pathname = "/";
    localStorage.clear();
    document.documentElement.lang = "";
    mockAssign.mockClear();
  });

  afterEach(() => {
    Object.defineProperty(window, "location", {
      configurable: true,
      writable: true,
      value: originalLocation,
    });
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("redirects a signed-in visitor to the account locale", async () => {
    router.pathname = "/";
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse({ language: "fr", updated_at: "t1" })),
    );

    render(<LocalePreferenceRoute />);

    await waitFor(() => expect(mockAssign).toHaveBeenCalledWith("/fr/"));
    expect(localStorage.getItem("newtg.language")).toBe("fr");
  });

  it("does not redirect an anonymous visitor who never chose", async () => {
    router.pathname = "/en/";
    const fetchMock = vi.fn(async (_input: RequestInfo | URL) => new Response(null, { status: 401 }));
    vi.stubGlobal("fetch", fetchMock);

    render(<LocalePreferenceRoute />);

    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    await act(async () => {
      await Promise.resolve();
    });
    expect(mockAssign).not.toHaveBeenCalled();
  });

  it("redirects an anonymous visitor who chose explicitly (Review Focus 5)", async () => {
    router.pathname = "/en/";
    localStorage.setItem("newtg.language", "zh-CN");
    localStorage.setItem("newtg.language.explicit", "1");
    vi.stubGlobal("fetch", vi.fn(async () => new Response(null, { status: 401 })));

    render(<LocalePreferenceRoute />);

    await waitFor(() => expect(mockAssign).toHaveBeenCalledWith("/"));
  });

  it("prefers a pending local choice without waiting for the network (Review Focus 3)", async () => {
    router.pathname = "/";
    localStorage.setItem("newtg.language", "ja");
    localStorage.setItem("newtg.language.pending", "1");
    vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>(() => {})));

    render(<LocalePreferenceRoute />);

    await waitFor(() => expect(mockAssign).toHaveBeenCalledWith("/ja/"));
  });

  it("stays put when the probe times out", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    router.pathname = "/";
    vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>(() => {})));

    render(<LocalePreferenceRoute />);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500);
    });
    expect(mockAssign).not.toHaveBeenCalled();
  });

  it("ignores non-marketing paths", async () => {
    router.pathname = "/dashboard";
    const fetchMock = vi.fn(async () => jsonResponse({ language: "fr", updated_at: "t1" }));
    vi.stubGlobal("fetch", fetchMock);

    render(<LocalePreferenceRoute />);

    await act(async () => {
      await Promise.resolve();
    });
    expect(fetchMock).not.toHaveBeenCalled();
    expect(mockAssign).not.toHaveBeenCalled();
  });

  it("sets document lang from the path", async () => {
    router.pathname = "/ja/pricing/";
    vi.stubGlobal("fetch", vi.fn(async () => new Response(null, { status: 401 })));

    render(<LocalePreferenceRoute />);

    await waitFor(() => expect(document.documentElement.lang).toBe("ja"));
  });
});
