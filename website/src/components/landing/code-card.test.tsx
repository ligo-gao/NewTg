import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { CodeCard } from "@/components/landing/code-card";

const CODE = "curl test";
const HTML = "<pre><code>curl test</code></pre>";

function renderCard() {
  return render(<CodeCard code={CODE} html={HTML} />);
}

function stubClipboard(writeText: (text: string) => Promise<void>) {
  Object.defineProperty(navigator, "clipboard", {
    value: { writeText },
    configurable: true,
  });
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("CodeCard", () => {
  it("copies the code and shows the copied label", async () => {
    const writeText = vi.fn<(text: string) => Promise<void>>().mockResolvedValue(undefined);
    stubClipboard(writeText);

    renderCard();
    fireEvent.click(screen.getByRole("button", { name: "复制" }));

    expect(writeText).toHaveBeenCalledWith(CODE);
    await waitFor(() => {
      expect(screen.getByRole("button")).toHaveTextContent("已复制");
    });
    expect(screen.getByRole("status")).toHaveTextContent("代码已复制到剪贴板");
  });

  it("reports a failure when the clipboard rejects", async () => {
    const writeText = vi
      .fn<(text: string) => Promise<void>>()
      .mockRejectedValue(new Error("denied"));
    stubClipboard(writeText);

    renderCard();
    fireEvent.click(screen.getByRole("button", { name: "复制" }));

    await waitFor(() => {
      expect(screen.getByRole("button")).toHaveTextContent("复制失败");
    });
    expect(screen.getByRole("status")).toHaveTextContent("复制失败，请手动选择代码");
  });
});
