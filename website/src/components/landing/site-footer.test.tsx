import { render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { getShellCopy } from "@newtg/public-shell";
import { copy } from "@/config/copy";
import {
  usePublicStatus,
  type PublicStatus,
  type PublicStatusState,
} from "@/lib/public-status";
import { SiteFooter } from "./site-footer";

vi.mock("@/lib/public-status", () => ({ usePublicStatus: vi.fn() }));

const usePublicStatusMock = vi.mocked(usePublicStatus);

/** The switch block a default deployment reports. */
const ALL_ON = {
  console: { enabled: true, require_auth: true },
  models: { enabled: true, require_auth: false },
  pricing: { enabled: true, require_auth: false },
  rankings: { enabled: true, require_auth: false },
  docs: { enabled: true, require_auth: false },
  about: { enabled: true, require_auth: false },
};

type Site = NonNullable<PublicStatus["site"]>;

function site(overrides: Site = {}): Site {
  return {
    title: "NEWTG",
    about_html: "",
    docs_url: "https://docs.newtg.ai",
    status_page_url: "",
    server_address: "",
    filing_no: "",
    ...overrides,
  };
}

function mockStatus({
  nav = ALL_ON,
  site: siteOverrides = site(),
  loading = false,
  error = undefined,
  docsEnabled,
  failed = false,
}: {
  nav?: typeof ALL_ON;
  site?: Site;
  loading?: boolean;
  error?: Error;
  docsEnabled?: boolean;
  failed?: boolean;
} = {}) {
  const docsUrl = siteOverrides.docs_url?.trim() ?? "";
  const state: PublicStatusState = {
    status: failed ? undefined : { nav, site: siteOverrides },
    loading,
    error,
    nav: failed ? undefined : nav,
    docsUrl: failed ? "" : docsUrl,
    docsEnabled:
      docsEnabled ??
      (!failed && !loading && nav.docs?.enabled !== false && docsUrl !== ""),
    statusPageUrl: failed ? "" : (siteOverrides.status_page_url?.trim() ?? ""),
    serverAddress: failed ? "" : (siteOverrides.server_address?.trim() ?? ""),
    siteTitle: failed ? "" : (siteOverrides.title?.trim() ?? ""),
  };
  usePublicStatusMock.mockReturnValue(state);
}

/** The column containing the given heading (product / resources / language). */
function column(name: string): HTMLElement {
  const heading = screen.getByRole("heading", { name });
  const element = heading.closest(".ps-footer-col");
  if (!(element instanceof HTMLElement)) throw new Error(`column "${name}" not found`);
  return element;
}

function linkPairs(container: HTMLElement): Array<[string | null, string | null]> {
  return within(container)
    .getAllByRole("link")
    .map((link) => [link.textContent, link.getAttribute("href")]);
}

beforeEach(() => {
  usePublicStatusMock.mockReset();
  mockStatus();
});

describe("SiteFooter", () => {
  it("renders the product column in the approved order with real hrefs", () => {
    const { container } = render(<SiteFooter copy={copy} locale="zh-CN" />);

    expect(linkPairs(column("产品"))).toEqual([
      ["模型库", "/models"],
      ["定价", "/pricing/"],
      ["排行", "/rankings"],
      ["关于", "/about"],
      ["控制台", "/dashboard"],
    ]);

    // Brand, product, resources, friendly links and the shell's language column.
    expect(container.querySelectorAll("h2")).toHaveLength(4);
    expect(screen.getByRole("heading", { name: "语言" })).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "语言" })).toHaveValue("zh-CN");
    expect(container.querySelector(".ps-brand")).toHaveAttribute("href", "/");
  });

  it("localizes pricing and the brand link for a prefixed locale", () => {
    const { container } = render(<SiteFooter copy={copy} locale="en" />);

    expect(linkPairs(column("Product"))).toEqual([
      ["Models", "/models"],
      ["Pricing", "/en/pricing/"],
      ["Rankings", "/rankings"],
      ["About", "/about"],
      ["Console", "/dashboard"],
    ]);
    expect(container.querySelector(".ps-brand")).toHaveAttribute("href", "/en/");
  });

  it("links docs, status, terms and privacy to their real destinations", () => {
    mockStatus({ site: site({ status_page_url: "https://status.newtg.ai" }) });
    render(<SiteFooter copy={copy} locale="zh-CN" />);

    const resources = column("资源");
    expect(linkPairs(resources)).toEqual([
      ["接口文档", "https://docs.newtg.ai"],
      ["状态页", "https://status.newtg.ai"],
      ["服务条款", "/legal/terms"],
      ["隐私政策", "/legal/privacy"],
    ]);

    const docs = within(resources).getByRole("link", { name: /接口文档/ });
    expect(docs).toHaveAttribute("target", "_blank");
    expect(docs).toHaveAttribute("rel", "noopener noreferrer");

    const status = within(resources).getByRole("link", { name: /状态页/ });
    expect(status).toHaveAttribute("target", "_blank");
  });

  it("hides the docs link when the status has no docs_url", () => {
    mockStatus({ site: site({ docs_url: "" }), docsEnabled: false });
    render(<SiteFooter copy={copy} locale="zh-CN" />);

    expect(screen.queryByRole("link", { name: /接口文档/ })).not.toBeInTheDocument();
    expect(linkPairs(column("资源"))).toEqual([
      ["服务条款", "/legal/terms"],
      ["隐私政策", "/legal/privacy"],
    ]);
  });

  it("hides the docs link when the console turned the entry off", () => {
    mockStatus({
      nav: { ...ALL_ON, docs: { enabled: false, require_auth: false } },
    });
    render(<SiteFooter copy={copy} locale="zh-CN" />);

    expect(screen.queryByRole("link", { name: /接口文档/ })).not.toBeInTheDocument();
  });

  it("hides the status link while unconfigured", () => {
    render(<SiteFooter copy={copy} locale="zh-CN" />);

    expect(screen.queryByRole("link", { name: /状态页/ })).not.toBeInTheDocument();
  });

  it("rejects an unsafe status target", () => {
    mockStatus({ site: site({ status_page_url: "javascript:alert(1)" }) });
    render(<SiteFooter copy={copy} locale="zh-CN" />);

    expect(screen.queryByRole("link", { name: /状态页/ })).not.toBeInTheDocument();
  });

  it("never renders placeholder hrefs and never repeats the docs link", () => {
    mockStatus({
      site: site({
        docs_url: "/docs",
        status_page_url: "https://status.newtg.ai",
        server_address: "https://gateway.example.com",
        filing_no: "京ICP备12345678号-1",
        about_html: "<p>统一接入，智能路由。</p>",
      }),
    });
    const { container } = render(<SiteFooter copy={copy} locale="zh-CN" />);

    const hrefs = Array.from(container.querySelectorAll("a")).map((link) =>
      link.getAttribute("href"),
    );
    for (const placeholder of ["#status", "#community", "#top"]) {
      expect(hrefs).not.toContain(placeholder);
    }
    expect(hrefs.filter((href) => href === "/docs")).toHaveLength(1);
  });

  it("lists the friendly AI links as real external destinations", () => {
    render(<SiteFooter copy={copy} locale="zh-CN" />);

    const friends = column("友情链接");
    const pairs = linkPairs(friends);
    expect(pairs[0]).toEqual(["OpenAI", "https://openai.com"]);
    expect(pairs.map(([label]) => label)).toEqual([
      "OpenAI",
      "Anthropic",
      "Google AI",
      "DeepSeek",
      "通义千问",
      "Hugging Face",
    ]);
    for (const [label, href] of pairs) {
      expect(href, label ?? "").toMatch(/^https:\/\//);
    }

    const openai = within(friends).getByRole("link", { name: /OpenAI/ });
    expect(openai).toHaveAttribute("target", "_blank");
    expect(openai).toHaveAttribute("rel", "noopener noreferrer");
  });

  it("follows the console switches for product entries", () => {
    mockStatus({
      nav: { ...ALL_ON, models: { enabled: false, require_auth: false } },
    });
    render(<SiteFooter copy={copy} locale="zh-CN" />);

    expect(screen.queryByRole("link", { name: "模型库" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "排行" })).toBeInTheDocument();
    // Console requires auth server-side, but the signpost stays visible.
    expect(screen.getByRole("link", { name: "控制台" })).toHaveAttribute(
      "href",
      "/dashboard",
    );
  });

  it("uses the plain text of the site statement", () => {
    mockStatus({ site: site({ about_html: "<p>统一接入，智能路由。</p>" }) });
    render(<SiteFooter copy={copy} locale="zh-CN" />);

    expect(screen.getByText("统一接入，智能路由。")).toBeInTheDocument();
  });

  it("falls back to the shared tagline when the statement is empty", () => {
    mockStatus({ site: site({ about_html: "<p>   </p>" }) });
    render(<SiteFooter copy={copy} locale="zh-CN" />);

    expect(
      screen.getByText(getShellCopy("zh-CN")["footer.tagline"]),
    ).toBeInTheDocument();
  });

  it("shows the API endpoint for a public server address", () => {
    mockStatus({ site: site({ server_address: "https://gateway.example.com/" }) });
    const { container } = render(<SiteFooter copy={copy} locale="zh-CN" />);

    expect(container.querySelector(".ps-footer-endpoint code")).toHaveTextContent(
      "https://gateway.example.com/v1",
    );
  });

  it.each([
    "http://localhost:3000",
    "http://127.0.0.1:8080",
    "http://[::1]:8080",
    "10.0.0.1:8080",
    "",
  ])("hides the endpoint for %s", (serverAddress) => {
    mockStatus({ site: site({ server_address: serverAddress }) });
    const { container } = render(<SiteFooter copy={copy} locale="zh-CN" />);

    expect(container.querySelector(".ps-footer-endpoint")).toBeNull();
  });

  it("renders the filing number linked to the MIIT registry when configured", () => {
    mockStatus({ site: site({ filing_no: "京ICP备12345678号-1" }) });
    const { container } = render(<SiteFooter copy={copy} locale="zh-CN" />);

    const filing = container.querySelector(".ps-footer-bottom a");
    expect(filing).toHaveAttribute("href", "https://beian.miit.gov.cn/");
    expect(filing).toHaveAttribute("target", "_blank");
    expect(filing).toHaveTextContent("京ICP备12345678号-1");
  });

  it("keeps the legal line and tagline when the status request fails", () => {
    mockStatus({ failed: true, error: new Error("status request failed: 500") });
    render(<SiteFooter copy={copy} locale="zh-CN" />);

    expect(screen.queryByRole("link", { name: /接口文档/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /状态页/ })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "服务条款" })).toHaveAttribute(
      "href",
      "/legal/terms",
    );
    expect(
      screen.getByText(getShellCopy("zh-CN")["footer.tagline"]),
    ).toBeInTheDocument();
  });
});
