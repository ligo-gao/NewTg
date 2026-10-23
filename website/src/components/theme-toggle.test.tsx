import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it } from "vitest";
import { copy } from "@/config/copy";
import { ThemeToggle } from "@/components/theme-toggle";
import { THEME_STORAGE_KEY } from "@/lib/theme";

describe("ThemeToggle", () => {
  beforeEach(() => {
    document.documentElement.dataset.theme = "light";
    document.documentElement.style.colorScheme = "light";
    localStorage.clear();
  });

  it("switches from light to dark and persists the choice", async () => {
    const user = userEvent.setup();
    render(<ThemeToggle copy={copy} />);

    await user.click(screen.getByRole("button", { name: "切换主题" }));

    expect(document.documentElement.dataset.theme).toBe("dark");
    expect(document.documentElement.style.colorScheme).toBe("dark");
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("dark");
  });

  it("switches back to light and persists the choice", async () => {
    document.documentElement.dataset.theme = "dark";
    document.documentElement.style.colorScheme = "dark";

    const user = userEvent.setup();
    render(<ThemeToggle copy={copy} />);

    await user.click(screen.getByRole("button", { name: "切换主题" }));

    expect(document.documentElement.dataset.theme).toBe("light");
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("light");
  });
});
