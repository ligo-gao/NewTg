import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { copy } from "@/config/copy";
import { SiteHeader } from "./site-header";

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
});
