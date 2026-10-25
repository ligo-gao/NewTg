import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
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

beforeEach(() => {
  usePublicStatusMock.mockReset();
  mockStatus();
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

    const docsLink = screen.getByRole("link", { name: "文档" });
    expect(docsLink).toHaveAttribute("href", "/docs");
  });

  it("hides the docs link when docs are not configured", () => {
    mockStatus({ docsEnabled: false });
    render(<SiteHeader copy={copy} locale="zh-CN" />);

    expect(screen.queryByRole("link", { name: "文档" })).not.toBeInTheDocument();
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
    expect(within(menu).queryByRole("link", { name: "文档" })).not.toBeInTheDocument();
    expect(within(menu).getByRole("link", { name: "模型库" })).toBeInTheDocument();
  });

  it("hides the docs link when the status request fails", () => {
    mockStatus({
      status: undefined,
      docsEnabled: false,
      error: new Error("status request failed: 500"),
    });
    render(<SiteHeader copy={copy} locale="zh-CN" />);

    expect(screen.queryByRole("link", { name: "文档" })).not.toBeInTheDocument();
  });
});
