import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { copy } from "@/config/copy";
import { usePublicStatus, type PublicStatusState } from "@/lib/public-status";
import { SiteHeader } from "./site-header";

vi.mock("@/lib/public-status", () => ({ usePublicStatus: vi.fn() }));

const usePublicStatusMock = vi.mocked(usePublicStatus);

function mockStatus(overrides: Partial<PublicStatusState> = {}) {
  usePublicStatusMock.mockReturnValue({
    status: { nav: { docs: { enabled: true } }, site: { docs_url: "https://docs.newtg.ai" } },
    loading: false,
    error: undefined,
    docsEnabled: true,
    ...overrides,
  });
}

/** `/api/me` is the only session probe the marketing header makes. */
function mockAccount(body: unknown, status = 200) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url === "/api/me") {
      return new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      });
    }
    return new Response(null, { status: 401 });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

beforeEach(() => {
  usePublicStatusMock.mockReset();
  mockStatus();
  mockAccount({}, 401);
});

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

describe("SiteHeader", () => {
  it("opens the mobile menu, then closes it when a link is clicked", async () => {
    const user = userEvent.setup();
    render(<SiteHeader copy={copy} locale="zh-CN" />);

    const toggle = screen.getByRole("button", { name: "打开导航菜单" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveAttribute("aria-controls", "mobile-menu");
    expect(document.getElementById("mobile-menu")).toBeNull();

    await user.click(toggle);

    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(toggle).toHaveAttribute("aria-label", "关闭导航菜单");

    const menu = document.getElementById("mobile-menu");
    expect(menu).not.toBeNull();
    if (!menu) throw new Error("the mobile menu did not render");

    const modelLink = within(menu).queryByRole("link", { name: "模型库" });
    expect(modelLink).toBeInTheDocument();
    if (!modelLink) throw new Error("the mobile menu is missing its links");

    await user.click(modelLink);

    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveAttribute("aria-label", "打开导航菜单");
    expect(document.getElementById("mobile-menu")).toBeNull();
  });

  it("points the model library link at the console /models page", async () => {
    const user = userEvent.setup();
    render(<SiteHeader copy={copy} locale="zh-CN" />);

    expect(screen.getByRole("link", { name: "模型库" })).toHaveAttribute("href", "/models");

    await user.click(screen.getByRole("button", { name: "打开导航菜单" }));
    const menu = document.getElementById("mobile-menu");
    expect(menu).not.toBeNull();
    if (!menu) throw new Error("the mobile menu did not render");
    expect(within(menu).getByRole("link", { name: "模型库" })).toHaveAttribute(
      "href",
      "/models",
    );
  });

  it("shows the docs link when the status reports docs configured", () => {
    mockStatus({ docsEnabled: true });
    render(<SiteHeader copy={copy} locale="zh-CN" />);

    const docsLink = screen.getByRole("link", { name: "接口文档" });
    expect(docsLink).toHaveAttribute("href", "/docs");
  });

  it("hides the docs link when docs are not configured", () => {
    mockStatus({ docsEnabled: false });
    render(<SiteHeader copy={copy} locale="zh-CN" />);

    expect(screen.queryByRole("link", { name: "接口文档" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "定价" })).toBeInTheDocument();
  });

  it("hides the docs link in the mobile menu too", async () => {
    const user = userEvent.setup();
    mockStatus({ docsEnabled: false });
    render(<SiteHeader copy={copy} locale="zh-CN" />);

    await user.click(screen.getByRole("button", { name: "打开导航菜单" }));

    const menu = document.getElementById("mobile-menu");
    expect(menu).not.toBeNull();
    if (!menu) throw new Error("the mobile menu did not render");
    expect(within(menu).queryByRole("link", { name: "接口文档" })).not.toBeInTheDocument();
    expect(within(menu).getByRole("link", { name: "模型库" })).toBeInTheDocument();
  });

  it("hides the docs link when the status request fails", () => {
    mockStatus({
      status: undefined,
      docsEnabled: false,
      error: new Error("status request failed: 500"),
    });
    render(<SiteHeader copy={copy} locale="zh-CN" />);

    expect(screen.queryByRole("link", { name: "接口文档" })).not.toBeInTheDocument();
  });

  it("shows the display name instead of the sign-in link when a session exists", async () => {
    mockAccount({ username: "ada", display_name: "Ada Lovelace" });
    render(<SiteHeader copy={copy} locale="zh-CN" />);

    expect(await screen.findByText("Ada Lovelace")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "登录" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /账户菜单/ })).toBeInTheDocument();
  });

  it("falls back to the username when display_name is empty", async () => {
    mockAccount({ username: "ada", display_name: null });
    render(<SiteHeader copy={copy} locale="zh-CN" />);

    expect(await screen.findByText("ada")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "登录" })).not.toBeInTheDocument();
  });

  it("stays a guest when the session probe answers 401", async () => {
    const fetchMock = mockAccount({}, 401);
    render(<SiteHeader copy={copy} locale="zh-CN" />);

    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(screen.getByRole("link", { name: "登录" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /账户菜单/ })).not.toBeInTheDocument();
  });

  it("opens the language menu from the globe instead of a select", async () => {
    mockAccount({}, 401);
    const originalLocation = window.location;
    const assign = vi.fn();
    Object.defineProperty(window, "location", {
      writable: true,
      value: { ...originalLocation, assign, pathname: "/" },
    });

    try {
      const user = userEvent.setup();
      render(<SiteHeader copy={copy} locale="zh-CN" />);

      expect(document.querySelector(".ps-lang select")).toBeNull();

      const globe = screen.getByRole("button", { name: /语言/ });
      await user.click(globe);

      const menu = screen.getByRole("menu", { name: "语言" });
      await user.click(within(menu).getByRole("menuitemradio", { name: "English" }));

      expect(assign).toHaveBeenCalledWith("/en/");
      expect(localStorage.getItem("newtg.language")).toBe("en");
    } finally {
      Object.defineProperty(window, "location", { writable: true, value: originalLocation });
    }
  });

  it("closes the language menu on Escape and returns focus to the globe", async () => {
    mockAccount({}, 401);
    const user = userEvent.setup();
    render(<SiteHeader copy={copy} locale="zh-CN" />);

    const globe = screen.getByRole("button", { name: /语言/ });
    await user.click(globe);
    expect(screen.getByRole("menu", { name: "语言" })).toBeInTheDocument();

    await user.keyboard("{Escape}");

    expect(screen.queryByRole("menu", { name: "语言" })).not.toBeInTheDocument();
    expect(globe).toHaveFocus();
  });
});
