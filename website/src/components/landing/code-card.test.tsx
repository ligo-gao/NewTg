import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { CodeCard, type CodeCardSample } from "@/components/landing/code-card";
import { copy } from "@/config/copy";

const SAMPLES: CodeCardSample[] = [
  {
    id: "curl",
    label: "cURL",
    code: "curl test",
    html: "<pre><code>curl test</code></pre>",
  },
  {
    id: "python",
    label: "Python",
    code: "print('hello')",
    html: "<pre><code>print('hello')</code></pre>",
  },
];

function renderCard(samples: CodeCardSample[] = SAMPLES) {
  return render(<CodeCard copy={copy} samples={samples} />);
}

function activeCard(): HTMLElement | null {
  return document.querySelector('[data-active="true"]');
}

function stubClipboard(writeText: (text: string) => Promise<void>) {
  Object.defineProperty(navigator, "clipboard", {
    value: { writeText },
    configurable: true,
  });
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("CodeCard", () => {
  it("renders one card per method plus a hidden loop clone", () => {
    const { container } = renderCard();

    const cards = container.querySelectorAll(".code-track > .code-card");
    expect(cards).toHaveLength(SAMPLES.length + 1);
    expect(cards[0]).toHaveAttribute("data-active", "true");
    expect(cards[1]).toHaveAttribute("data-active", "false");

    const clone = cards[cards.length - 1];
    expect(clone).toHaveAttribute("aria-hidden", "true");
    clone.querySelectorAll("button").forEach((button) => {
      expect(button).toHaveAttribute("tabindex", "-1");
    });
  });

  it("copies the code of the card whose button was pressed", async () => {
    const writeText = vi.fn<(text: string) => Promise<void>>().mockResolvedValue(undefined);
    stubClipboard(writeText);

    renderCard();
    fireEvent.click(
      screen.getByRole("button", { name: `${copy.hero.copyLabel} Python` }),
    );

    expect(writeText).toHaveBeenCalledWith("print('hello')");
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: `${copy.hero.copyLabel} Python` }),
      ).toHaveTextContent(copy.hero.copiedLabel);
    });
    const pythonCard = screen.getByRole("article", { name: "Python" });
    expect(within(pythonCard).getByRole("status")).toHaveTextContent(
      copy.hero.copySuccessAnnouncement,
    );
  });

  it("reports a failure when the clipboard rejects", async () => {
    const writeText = vi
      .fn<(text: string) => Promise<void>>()
      .mockRejectedValue(new Error("denied"));
    stubClipboard(writeText);

    renderCard();
    fireEvent.click(
      screen.getByRole("button", { name: `${copy.hero.copyLabel} cURL` }),
    );

    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: `${copy.hero.copyLabel} cURL` }),
      ).toHaveTextContent(copy.hero.copyFailedLabel);
    });
    const curlCard = screen.getByRole("article", { name: "cURL" });
    expect(within(curlCard).getByRole("status")).toHaveTextContent(
      copy.hero.copyFailureAnnouncement,
    );
  });

  it("advances to the next method after the dwell time and wraps around", () => {
    vi.useFakeTimers();
    renderCard();

    expect(activeCard()).toHaveAttribute("aria-label", "cURL");

    act(() => {
      vi.advanceTimersByTime(5600);
    });
    expect(activeCard()).toHaveAttribute("aria-label", "Python");

    act(() => {
      vi.advanceTimersByTime(5600);
    });
    // The clone (identical to the first card) is reached, then rewound.
    act(() => {
      vi.advanceTimersByTime(1000);
    });
    expect(activeCard()).toHaveAttribute("aria-label", "cURL");
  });

  it("pauses while the pointer is over the carousel", () => {
    vi.useFakeTimers();
    renderCard();

    const carousel = document.querySelector(".code-carousel");
    if (!(carousel instanceof HTMLElement)) {
      throw new Error("missing carousel");
    }
    fireEvent.pointerEnter(carousel);

    act(() => {
      vi.advanceTimersByTime(20000);
    });
    expect(activeCard()).toHaveAttribute("aria-label", "cURL");
  });
});
