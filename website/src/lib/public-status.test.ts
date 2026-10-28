import { renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { resetPublicStatusCache, resolveDocsEnabled, usePublicStatus } from "./public-status";

const fetchMock = vi.fn();

function jsonResponse(body: unknown, ok = true, status = 200): Response {
  return {
    ok,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

beforeEach(() => {
  // 缓存是模块级的，测试之间必须显式清空，否则后一个用例拿不到自己的响应。
  resetPublicStatusCache();
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("resolveDocsEnabled", () => {
  it("requires both an enabled nav entry and a non-empty docs_url", () => {
    expect(resolveDocsEnabled(undefined)).toBe(false);
    expect(resolveDocsEnabled({ nav: { docs: { enabled: true } } })).toBe(false);
    expect(resolveDocsEnabled({ site: { docs_url: "https://docs.newtg.ai" } })).toBe(true);
    expect(
      resolveDocsEnabled({ nav: { docs: { enabled: false } }, site: { docs_url: "https://docs.newtg.ai" } }),
    ).toBe(false);
    expect(resolveDocsEnabled({ site: { docs_url: "" } })).toBe(false);
    expect(resolveDocsEnabled({ site: { docs_url: "   " } })).toBe(false);
  });
});

describe("usePublicStatus", () => {
  it("keeps docs disabled while the status request is in flight", () => {
    fetchMock.mockReturnValue(new Promise(() => undefined));

    const { result } = renderHook(() => usePublicStatus());

    expect(result.current.loading).toBe(true);
    expect(result.current.docsEnabled).toBe(false);
    expect(result.current.error).toBeUndefined();
  });

  it("fetches /api/status once without caching and enables docs", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({
        nav: { docs: { enabled: true, require_auth: false } },
        site: { docs_url: "https://docs.newtg.ai" },
      }),
    );

    const { result } = renderHook(() => usePublicStatus());

    await waitFor(() => expect(result.current.loading).toBe(false));

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/status",
      expect.objectContaining({ cache: "no-store" }),
    );
    expect(result.current.error).toBeUndefined();
    expect(result.current.status?.site?.docs_url).toBe("https://docs.newtg.ai");
    expect(result.current.docsEnabled).toBe(true);
  });

  it("keeps docs disabled when the deployment has no docs_url", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ nav: { docs: { enabled: true } }, site: { docs_url: "" } }),
    );

    const { result } = renderHook(() => usePublicStatus());

    await waitFor(() => expect(result.current.loading).toBe(false));

    expect(result.current.docsEnabled).toBe(false);
  });

  it("keeps docs disabled when the nav entry is disabled", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({
        nav: { docs: { enabled: false } },
        site: { docs_url: "https://docs.newtg.ai" },
      }),
    );

    const { result } = renderHook(() => usePublicStatus());

    await waitFor(() => expect(result.current.loading).toBe(false));

    expect(result.current.docsEnabled).toBe(false);
  });

  it("reports an error and keeps docs disabled when the request fails", async () => {
    fetchMock.mockRejectedValue(new Error("network down"));

    const { result } = renderHook(() => usePublicStatus());

    await waitFor(() => expect(result.current.loading).toBe(false));

    expect(result.current.error).toBeInstanceOf(Error);
    expect(result.current.status).toBeUndefined();
    expect(result.current.docsEnabled).toBe(false);
  });

  it("treats a non-ok response as an error", async () => {
    fetchMock.mockResolvedValue(jsonResponse({}, false, 503));

    const { result } = renderHook(() => usePublicStatus());

    await waitFor(() => expect(result.current.loading).toBe(false));

    expect(result.current.error?.message).toContain("503");
    expect(result.current.docsEnabled).toBe(false);
  });

  it("shares one /api/status request across every consumer on the page", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({
        nav: { docs: { enabled: true } },
        site: { docs_url: "https://docs.newtg.ai" },
      }),
    );

    // header、footer、hero CTA 在首页同时挂载；三个 hook 只允许打一次接口。
    const header = renderHook(() => usePublicStatus());
    const footer = renderHook(() => usePublicStatus());
    const hero = renderHook(() => usePublicStatus());

    await waitFor(() => expect(header.result.current.loading).toBe(false));
    await waitFor(() => expect(footer.result.current.loading).toBe(false));
    await waitFor(() => expect(hero.result.current.loading).toBe(false));

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(footer.result.current.docsEnabled).toBe(true);
    expect(hero.result.current.docsEnabled).toBe(true);
  });

  it("reuses the settled response for consumers mounted later", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ nav: { docs: { enabled: true } }, site: { docs_url: "" } }),
    );

    const first = renderHook(() => usePublicStatus());
    await waitFor(() => expect(first.result.current.loading).toBe(false));
    first.unmount();

    const second = renderHook(() => usePublicStatus());

    expect(second.result.current.loading).toBe(false);
    expect(second.result.current.status).toBeDefined();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
