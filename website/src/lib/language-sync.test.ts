import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  fetchAccountLocale,
  resetAccountLocaleCache,
  syncLanguageToAccount,
} from "./language-sync";

const CSRF_COOKIE = "__Host-nkt_csrf";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function setCsrf(token: string): void {
  // jsdom 只在带 Secure/Path 的前缀规则时才接受 __Host- Cookie。
  document.cookie = `${CSRF_COOKIE}=${token}; Secure; Path=/`;
}

function clearCsrf(): void {
  document.cookie = `${CSRF_COOKIE}=; Max-Age=0; Secure; Path=/`;
}

function calls(fetchMock: ReturnType<typeof vi.fn>): string[] {
  return fetchMock.mock.calls.map(([input, init]) => {
    const method = (init as RequestInit | undefined)?.method ?? "GET";
    return `${method} ${String(input)}`;
  });
}

describe("syncLanguageToAccount", () => {
  beforeEach(() => {
    localStorage.clear();
    resetAccountLocaleCache();
    clearCsrf();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    clearCsrf();
  });

  it("refreshes a missing CSRF cookie before writing", async () => {
    localStorage.setItem("newtg.language.pending", "1");
    let putHeaders: Headers | undefined;
    let putKeepalive: boolean | undefined;
    const fetchMock = vi.fn(
      async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const url = String(input);
        if (url === "/api/self") return jsonResponse({ language: "en", updated_at: "t1" });
        if (url === "/api/me") {
          setCsrf("fresh");
          return jsonResponse({});
        }
        if (url === "/api/self/setting") {
          putHeaders = new Headers(init?.headers);
          putKeepalive = init?.keepalive;
          return jsonResponse({ language: "en", updated_at: "t2" });
        }
        throw new Error(`unexpected fetch: ${url}`);
      },
    );
    vi.stubGlobal("fetch", fetchMock);

    await expect(syncLanguageToAccount("en")).resolves.toBe("saved");

    expect(putHeaders?.get("X-CSRF-Token")).toBe("fresh");
    expect(putKeepalive).toBe(true);
    expect(calls(fetchMock)).toEqual([
      "GET /api/self",
      "GET /api/me",
      "PUT /api/self/setting",
    ]);
    expect(localStorage.getItem("newtg.language.pending")).toBeNull();
  });

  it("retries exactly once after a csrf_failed 403", async () => {
    localStorage.setItem("newtg.language.pending", "1");
    setCsrf("stale");
    let putCount = 0;
    const retryTokens: Array<string | null> = [];
    const fetchMock = vi.fn(
      async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const url = String(input);
        if (url === "/api/me") {
          setCsrf("rotated");
          return jsonResponse({});
        }
        if (url === "/api/self/setting") {
          putCount += 1;
          retryTokens.push(new Headers(init?.headers).get("X-CSRF-Token"));
          if (putCount === 1) {
            return jsonResponse({ type: "https://newtg.dev/problems/csrf_failed" }, 403);
          }
          return jsonResponse({ language: "en", updated_at: "t2" });
        }
        throw new Error(`unexpected fetch: ${url}`);
      },
    );
    vi.stubGlobal("fetch", fetchMock);

    // 预热：直接给缓存令牌，验证 PUT 路径本身（不经过探测）。
    await primeCache(fetchMock, "t1");

    await expect(syncLanguageToAccount("en")).resolves.toBe("saved");

    expect(putCount).toBe(2);
    expect(retryTokens).toEqual(["stale", "rotated"]);
    // 恰好一次刷新 + 恰好一次重试，没有多余的探测。
    expect(calls(fetchMock)).toEqual([
      "PUT /api/self/setting",
      "GET /api/me",
      "PUT /api/self/setting",
    ]);
    expect(localStorage.getItem("newtg.language.pending")).toBeNull();
  });

  it("returns failed without writing when the probe is not a usable 200", async () => {
    const failures = [
      vi.fn(
        async (_input: RequestInfo | URL, _init?: RequestInit): Promise<Response> =>
          new Response(null, { status: 500 }),
      ),
      vi.fn(
        async (_input: RequestInfo | URL, _init?: RequestInit): Promise<Response> =>
          jsonResponse({ language: "de", updated_at: "t1" }),
      ),
      vi.fn(
        async (_input: RequestInfo | URL, _init?: RequestInit): Promise<Response> => {
          throw new TypeError("Failed to fetch");
        },
      ),
    ];

    for (const fetchMock of failures) {
      resetAccountLocaleCache();
      localStorage.setItem("newtg.language.pending", "1");
      vi.stubGlobal("fetch", fetchMock);

      await expect(syncLanguageToAccount("en")).resolves.toBe("failed");
      expect(
        fetchMock.mock.calls.some(([, init]) => (init as RequestInit | undefined)?.method === "PUT"),
      ).toBe(false);
      expect(localStorage.getItem("newtg.language.pending")).toBe("1");
      vi.unstubAllGlobals();
    }
  });

  it("issues the PUT synchronously from the primed cache, without a GET probe", async () => {
    const primeMock = vi.fn(async () => jsonResponse({ language: "en", updated_at: "t1" }));
    vi.stubGlobal("fetch", primeMock);
    await expect(fetchAccountLocale()).resolves.toEqual({
      status: "ok",
      language: "en",
      updated_at: "t1",
    });
    expect(primeMock).toHaveBeenCalledTimes(1);

    setCsrf("tok");
    localStorage.setItem("newtg.language.pending", "1");
    const putMock = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit): Promise<Response> =>
        jsonResponse({ language: "en", updated_at: "t2" }),
    );
    vi.stubGlobal("fetch", putMock);

    const started = syncLanguageToAccount("en");
    // 关键断言：调用尚未 await，PUT 已经在途——keepalive 才可能活过 location.assign。
    expect(calls(putMock)).toEqual(["PUT /api/self/setting"]);

    await expect(started).resolves.toBe("saved");
    expect(calls(putMock)).toEqual(["PUT /api/self/setting"]);
    expect(localStorage.getItem("newtg.language.pending")).toBeNull();
  });

  it("re-probes once and retries once when the cached token is stale", async () => {
    let probeCount = 0;
    let putCount = 0;
    const fetchMock = vi.fn(
      async (input: RequestInfo | URL, _init?: RequestInit): Promise<Response> => {
        const url = String(input);
        if (url === "/api/self") {
          probeCount += 1;
          return jsonResponse({
            language: "en",
            updated_at: probeCount === 1 ? "t1" : "fresh",
          });
        }
        if (url === "/api/self/setting") {
          putCount += 1;
          return putCount === 1
            ? jsonResponse({ type: "https://newtg.dev/problems/conflict" }, 409)
            : jsonResponse({ language: "en", updated_at: "t3" });
        }
        throw new Error(`unexpected fetch: ${url}`);
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    setCsrf("tok");
    localStorage.setItem("newtg.language.pending", "1");

    await fetchAccountLocale(); // 预热 t1

    await expect(syncLanguageToAccount("en")).resolves.toBe("saved");

    expect(putCount).toBe(2);
    expect(probeCount).toBe(2);
    expect(localStorage.getItem("newtg.language.pending")).toBeNull();
  });

  it("gives up after one stale retry instead of looping", async () => {
    const fetchMock = vi.fn(
      async (input: RequestInfo | URL, _init?: RequestInit): Promise<Response> => {
        const url = String(input);
        if (url === "/api/self") return jsonResponse({ language: "en", updated_at: "fresh" });
        if (url === "/api/self/setting") {
          return jsonResponse({ type: "https://newtg.dev/problems/conflict" }, 409);
        }
        throw new Error(`unexpected fetch: ${url}`);
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    setCsrf("tok");
    localStorage.setItem("newtg.language.pending", "1");

    await fetchAccountLocale(); // 预热

    await expect(syncLanguageToAccount("en")).resolves.toBe("failed");

    expect(calls(fetchMock).filter((call) => call === "PUT /api/self/setting")).toHaveLength(2);
    expect(localStorage.getItem("newtg.language.pending")).toBe("1");
  });
});

/** 让模块级令牌缓存持有 `updatedAt`，不动被测的 sync 路径。 */
async function primeCache(fetchMock: ReturnType<typeof vi.fn>, updatedAt: string): Promise<void> {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => jsonResponse({ language: "en", updated_at: updatedAt })),
  );
  await fetchAccountLocale();
  vi.stubGlobal("fetch", fetchMock);
}
