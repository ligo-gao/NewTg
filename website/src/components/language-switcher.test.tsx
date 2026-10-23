import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { LanguageSwitcher } from "@/components/language-switcher";
import { LOCALES } from "@/config/locales";

afterEach(() => {
  vi.restoreAllMocks();
  localStorage.clear();
});

describe("LanguageSwitcher", () => {
  it("lists every locale and navigates to the chosen static route", async () => {
    const assign = vi.fn();
    const originalLocation = window.location;
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { ...originalLocation, assign },
    });

    const user = userEvent.setup();
    render(<LanguageSwitcher locale="zh-CN" label="语言" />);

    const select = screen.getByRole("combobox", { name: "语言" }) as HTMLSelectElement;
    expect(select.options).toHaveLength(LOCALES.length);
    expect(select.value).toBe("zh-CN");

    await user.selectOptions(select, "fr");

    expect(localStorage.getItem("newtg-locale")).toBe("fr");
    expect(assign).toHaveBeenCalledWith("/fr/");

    Object.defineProperty(window, "location", {
      configurable: true,
      value: originalLocation,
    });
  });

  it("keeps the short label over the native menu in compact mode", () => {
    render(<LanguageSwitcher locale="ja" label="言語" compact />);
    expect(screen.getByText("JA")).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "言語" })).toHaveValue("ja");
  });
});
