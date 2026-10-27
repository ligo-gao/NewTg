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
    document.cookie = "__Host-nkt_csrf=; Max-Age=0; Secure; Path=/";
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
    document.cookie = "__Host-nkt_csrf=; Max-Age=0; Secure; Path=/";
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
    let resolveProbe: ((response: Response) => void) | undefined;
    const fetchMock = vi.fn(
      () =>
        new Promise<Response>((resolve) => {
          resolveProbe = resolve;
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    render(<LocalePreferenceRoute />);

    // 先确认探测已经发出，再让 401 落地；跨过一个宏任务后 decide() 的整条
    // 异步链必然结束，此时断言「没有跳转」才不是早跑的空断言。
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    await act(async () => {
      resolveProbe?.(new Response(null, { status: 401 }));
      await new Promise((resolve) => setTimeout(resolve, 0));
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

  it("clears a pending marker whose locale is unreadable and follows the account", async () => {
    router.pathname = "/";
    localStorage.setItem("newtg.language.pending", "1");
    // 非法值：readStoredLocale 返回 null。守卫不能永远停在 pending 分支，
    // 必须清掉标记后继续探测账号。
    localStorage.setItem("newtg.language", "de");
    const fetchMock = vi.fn(async () => jsonResponse({ language: "fr", updated_at: "t1" }));
    vi.stubGlobal("fetch", fetchMock);

    render(<LocalePreferenceRoute />);

    await waitFor(() => expect(mockAssign).toHaveBeenCalledWith("/fr/"));
    expect(fetchMock).toHaveBeenCalledWith("/api/self", expect.anything());
    expect(localStorage.getItem("newtg.language.pending")).toBeNull();
  });

  it("retries a pending write even when the URL already matches the local choice", async () => {
    router.pathname = "/ja/";
    localStorage.setItem("newtg.language", "ja");
    localStorage.setItem("newtg.language.pending", "1");
    document.cookie = "__Host-nkt_csrf=tok; Secure; Path=/";
    const fetchMock = vi.fn(
      async (input: RequestInfo | URL, _init?: RequestInit): Promise<Response> =>
        String(input) === "/api/self"
          ? jsonResponse({ language: "ja", updated_at: "t1" })
          : jsonResponse({ language: "ja", updated_at: "t2" }),
    );
    vi.stubGlobal("fetch", fetchMock);

    render(<LocalePreferenceRoute />);

    // 不跳转（URL 已一致），但必须补写一次，pending 才能在营销站内收敛。
    await waitFor(() => expect(localStorage.getItem("newtg.language.pending")).toBeNull());
    expect(mockAssign).not.toHaveBeenCalled();
  });

  it("stays put when the probe times out", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    router.pathname = "/";
    const signals: AbortSignal[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
        if (init?.signal) signals.push(init.signal);
        return new Promise<Response>(() => {});
      }),
    );

    render(<LocalePreferenceRoute />);
    await act(async () => {
      await Promise.resolve();
    });
    expect(signals).toHaveLength(1);
    expect(signals[0].aborted).toBe(false);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500);
    });

    // 超时必须真的 abort 掉探测，而不是永远挂起。
    expect(signals[0].aborted).toBe(true);
    expect(mockAssign).not.toHaveBeenCalled();
    expect(window.location.pathname).toBe("/");
    expect(document.documentElement.lang).toBe("zh-CN");
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

  it("stamps the schema version on mount without fabricating a choice", async () => {
    router.pathname = "/";
    vi.stubGlobal("fetch", vi.fn(async () => new Response(null, { status: 401 })));

    render(<LocalePreferenceRoute />);

    await waitFor(() => expect(localStorage.getItem("newtg.language.schema")).toBe("2"));
    expect(localStorage.getItem("newtg.language.explicit")).toBeNull();
    expect(localStorage.getItem("newtg.language.pending")).toBeNull();
  });

  it("sets document lang from the path", async () => {
    router.pathname = "/ja/pricing/";
    vi.stubGlobal("fetch", vi.fn(async () => new Response(null, { status: 401 })));

    render(<LocalePreferenceRoute />);

    await waitFor(() => expect(document.documentElement.lang).toBe("ja"));
  });
});
