import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { copy } from "@/config/copy";
import { usePublicStatus, type PublicStatusState } from "@/lib/public-status";
import { SiteFooter } from "./site-footer";

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

describe("SiteFooter", () => {
  it("renders the docs column when the status reports docs configured", () => {
    const { container } = render(<SiteFooter copy={copy} locale="zh-CN" />);

    expect(screen.getByRole("heading", { name: "文档" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "快速开始" })).toHaveAttribute("href", "/docs");
    expect(screen.getByRole("link", { name: "API 参考" })).toHaveAttribute("href", "/docs");
    // Four link columns plus the language column the shared footer adds.
    expect(container.querySelectorAll("h2")).toHaveLength(5);
    const grid = container.querySelector<HTMLElement>(".ps-footer-grid");
    expect(grid?.style.getPropertyValue("--ps-footer-grid")).toContain("repeat(5,");
  });

  it("omits the docs column and settles the grid when docs are not configured", () => {
    mockStatus({
      status: { nav: { docs: { enabled: true } }, site: { docs_url: "" } },
      docsEnabled: false,
    });
    const { container } = render(<SiteFooter copy={copy} locale="zh-CN" />);

    expect(screen.queryByRole("heading", { name: "文档" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "快速开始" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "API 参考" })).not.toBeInTheDocument();

    // The remaining columns still render; only the docs column is dropped.
    expect(screen.getByRole("heading", { name: "产品" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "社区" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "法律" })).toBeInTheDocument();
    expect(container.querySelectorAll("h2")).toHaveLength(4);
    const grid = container.querySelector<HTMLElement>(".ps-footer-grid");
    expect(grid?.style.getPropertyValue("--ps-footer-grid")).toContain("repeat(4,");
  });

  it("omits the docs links when the status request fails", () => {
    mockStatus({
      status: undefined,
      docsEnabled: false,
      error: new Error("status request failed: 500"),
    });
    render(<SiteFooter copy={copy} locale="zh-CN" />);

    expect(screen.queryByRole("link", { name: "快速开始" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "API 参考" })).not.toBeInTheDocument();
  });
});
