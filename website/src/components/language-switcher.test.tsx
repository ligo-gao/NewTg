import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { userEvent } from "@testing-library/user-event";
import { LanguageSwitcher } from "./language-switcher";

describe("LanguageSwitcher", () => {
  const mockAssign = vi.fn();
  let originalLocation: Location;

  beforeEach(() => {
    originalLocation = window.location;
    Object.defineProperty(window, "location", {
      writable: true,
      value: {
        ...originalLocation,
        assign: mockAssign,
        pathname: "/",
      },
    });
    localStorage.clear();
  });

  afterEach(() => {
    Object.defineProperty(window, "location", {
      writable: true,
      value: originalLocation,
    });
    mockAssign.mockClear();
    vi.unstubAllGlobals();
    // jsdom only accepts a `__Host-` cookie when the prefix rules are spelled
    // out in the same setter call, so expire it the same way.
    document.cookie = "__Host-nkt_csrf=; Max-Age=0; Secure; Path=/";
  });

  it("renders with current locale", () => {
    render(<LanguageSwitcher locale="zh-CN" label="语言" />);
    const select = screen.getByLabelText("语言");
    expect(select).toHaveValue("zh-CN");
  });

  it("switches from zh-CN homepage to en homepage", async () => {
    const user = userEvent.setup();
    window.location.pathname = "/";

    render(<LanguageSwitcher locale="zh-CN" label="语言" />);
    const select = screen.getByLabelText("语言");

    await user.selectOptions(select, "en");

    expect(mockAssign).toHaveBeenCalledWith("/en/");
    expect(localStorage.getItem("newtg.language")).toBe("en");
  });

  it("switches from zh-CN pricing to en pricing", async () => {
    const user = userEvent.setup();
    window.location.pathname = "/pricing/";

    render(<LanguageSwitcher locale="zh-CN" label="语言" />);
    const select = screen.getByLabelText("语言");

    await user.selectOptions(select, "en");

    expect(mockAssign).toHaveBeenCalledWith("/en/pricing/");
  });

  it("switches from en homepage to zh-CN homepage", async () => {
    const user = userEvent.setup();
    window.location.pathname = "/en/";

    render(<LanguageSwitcher locale="en" label="Language" />);
    const select = screen.getByLabelText("Language");

    await user.selectOptions(select, "zh-CN");

    expect(mockAssign).toHaveBeenCalledWith("/");
  });

  it("switches from en pricing to zh-CN pricing", async () => {
    const user = userEvent.setup();
    window.location.pathname = "/en/pricing/";

    render(<LanguageSwitcher locale="en" label="Language" />);
    const select = screen.getByLabelText("Language");

    await user.selectOptions(select, "zh-CN");

    expect(mockAssign).toHaveBeenCalledWith("/pricing/");
  });

  it("switches from fr pricing to ja pricing", async () => {
    const user = userEvent.setup();
    window.location.pathname = "/fr/pricing/";

    render(<LanguageSwitcher locale="fr" label="Langue" />);
    const select = screen.getByLabelText("Langue");

    await user.selectOptions(select, "ja");

    expect(mockAssign).toHaveBeenCalledWith("/ja/pricing/");
  });

  it("handles localStorage unavailable gracefully", async () => {
    const user = userEvent.setup();
    window.location.pathname = "/";

    const setItemSpy = vi.spyOn(Storage.prototype, "setItem");
    setItemSpy.mockImplementation(() => {
      throw new Error("Storage unavailable");
    });

    render(<LanguageSwitcher locale="zh-CN" label="语言" />);
    const select = screen.getByLabelText("语言");

    await user.selectOptions(select, "en");

    // Should still navigate even if localStorage fails
    expect(mockAssign).toHaveBeenCalledWith("/en/");

    setItemSpy.mockRestore();
  });

  it("renders compact variant", () => {
    render(<LanguageSwitcher locale="en" label="Language" compact />);
    expect(screen.getByText("EN")).toBeInTheDocument();
  });

  it("renders non-compact variant with icon", () => {
    render(<LanguageSwitcher locale="zh-CN" label="语言" />);
    const select = screen.getByLabelText("语言");
    expect(select).toBeInTheDocument();
    // 验证 select 在带图标的容器内
    expect(select.parentElement?.querySelector("svg")).toBeInTheDocument();
  });

  it("marks the choice explicit and pending before navigating", async () => {
    const user = userEvent.setup();
    window.location.pathname = "/";
    // 探测挂起：只关心手势写入的两个标记。
    vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>(() => {})));

    render(<LanguageSwitcher locale="zh-CN" label="语言" />);
    const select = screen.getByLabelText("语言");

    await user.selectOptions(select, "en");

    expect(localStorage.getItem("newtg.language.explicit")).toBe("1");
    expect(localStorage.getItem("newtg.language.pending")).toBe("1");
  });

  it("navigates first and syncs in the background with the CSRF header", async () => {
    const user = userEvent.setup();
    window.location.pathname = "/";
    document.cookie = "__Host-nkt_csrf=tok; Secure; Path=/";

    let putHeaders: Headers | undefined;
    let putBody: unknown;
    const fetchMock = vi.fn(
      async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const url = String(input);
        if (url === "/api/self") {
          return new Response(JSON.stringify({ language: "en", updated_at: "t1" }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          });
        }
        if (url === "/api/self/setting") {
          putHeaders = new Headers(init?.headers);
          putBody = JSON.parse(String(init?.body));
          return new Response(JSON.stringify({ language: "en", updated_at: "t2" }), {
            status: 200,
          });
        }
        throw new Error(`unexpected fetch: ${url}`);
      },
    );
    vi.stubGlobal("fetch", fetchMock);

    render(<LanguageSwitcher locale="zh-CN" label="语言" />);
    const select = screen.getByLabelText("语言");

    await user.selectOptions(select, "en");

    expect(mockAssign).toHaveBeenCalledWith("/en/");
    await waitFor(() => expect(putHeaders?.get("X-CSRF-Token")).toBe("tok"));
    expect(putBody).toEqual({ language: "en", updated_at: "t1" });
    await waitFor(() => expect(localStorage.getItem("newtg.language.pending")).toBeNull());
  });

  it("keeps pending when the sync request fails (Review Focus 2)", async () => {
    const user = userEvent.setup();
    window.location.pathname = "/";

    const fetchMock = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit): Promise<Response> =>
        new Response(null, { status: 401 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    render(<LanguageSwitcher locale="zh-CN" label="语言" />);
    const select = screen.getByLabelText("语言");

    await user.selectOptions(select, "en");

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/self",
      expect.objectContaining({ credentials: "same-origin" }),
    );
    const putCalls = fetchMock.mock.calls.filter(
      ([, init]) => (init as RequestInit | undefined)?.method === "PUT",
    );
    expect(putCalls).toHaveLength(0);
    expect(localStorage.getItem("newtg.language.pending")).toBe("1");
  });
});
