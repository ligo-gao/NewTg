import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { copy } from "@/config/copy";
import { usePublicStatus, type PublicStatusState } from "@/lib/public-status";
import { HeroDocsCta } from "./hero-docs-cta";

vi.mock("@/lib/public-status", () => ({ usePublicStatus: vi.fn() }));

const usePublicStatusMock = vi.mocked(usePublicStatus);

function mockStatus(overrides: Partial<PublicStatusState> = {}) {
  usePublicStatusMock.mockReturnValue({
    status: { nav: { docs: { enabled: true } }, site: { docs_url: "https://docs.newtg.ai" } },
    loading: false,
    error: undefined,
    nav: { docs: { enabled: true } },
    docsUrl: "https://docs.newtg.ai",
    docsEnabled: true,
    statusPageUrl: "",
    serverAddress: "",
    siteTitle: "",
    ...overrides,
  });
}

beforeEach(() => {
  usePublicStatusMock.mockReset();
  mockStatus();
});

describe("HeroDocsCta", () => {
  it("renders the docs CTA when docs are enabled", () => {
    mockStatus({ docsEnabled: true });
    render(<HeroDocsCta copy={copy} />);

    const link = screen.getByRole("link", { name: "查看文档" });
    expect(link).toHaveAttribute("href", "/docs");
    expect(link).toHaveClass("btn", "btn-ghost");
  });

  it("renders nothing when docs are not configured", () => {
    mockStatus({
      status: { nav: { docs: { enabled: true } }, site: { docs_url: "" } },
      docsEnabled: false,
    });
    const { container } = render(<HeroDocsCta copy={copy} />);

    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing while the status is still loading", () => {
    mockStatus({ status: undefined, loading: true, docsEnabled: false });
    const { container } = render(<HeroDocsCta copy={copy} />);

    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when the status request fails", () => {
    mockStatus({
      status: undefined,
      docsEnabled: false,
      error: new Error("status request failed: 500"),
    });
    const { container } = render(<HeroDocsCta copy={copy} />);

    expect(container).toBeEmptyDOMElement();
  });
});
