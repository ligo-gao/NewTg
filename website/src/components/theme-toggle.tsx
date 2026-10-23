"use client";

import { useEffect } from "react";
import type { Copy } from "@/config/copy";
import { applyTheme, readStoredTheme, storeTheme } from "@/lib/theme";

export function ThemeToggle({ copy, className = "" }: { copy: Copy; className?: string }) {
  useEffect(() => {
    const media = window.matchMedia?.("(prefers-color-scheme: dark)");
    if (!media) {
      return;
    }

    const onChange = (event: MediaQueryListEvent) => {
      // An explicit choice wins; only follow the OS when nothing is stored.
      if (readStoredTheme() !== null) {
        return;
      }
      applyTheme(event.matches ? "dark" : "light");
    };

    media.addEventListener("change", onChange);
    return () => media.removeEventListener("change", onChange);
  }, []);

  function toggleTheme() {
    const current = document.documentElement.dataset.theme === "dark" ? "dark" : "light";
    const next = current === "dark" ? "light" : "dark";
    applyTheme(next);
    storeTheme(next);
  }

  return (
    <button
      type="button"
      onClick={toggleTheme}
      aria-label={copy.theme.toggle}
      title={copy.theme.toggle}
      className={`inline-flex size-9 flex-none items-center justify-center rounded-md border border-line-2 bg-transparent text-muted transition-colors hover:border-cyan-dim hover:text-ink ${className}`}
    >
      <svg
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.7"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
        className="size-4 dark:hidden"
      >
        <path d="M20.5 14.6A8.5 8.5 0 0 1 9.4 3.5a8.5 8.5 0 1 0 11.1 11.1Z" />
      </svg>
      <svg
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.7"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
        className="hidden size-4 dark:block"
      >
        <circle cx="12" cy="12" r="4" />
        <path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
      </svg>
    </button>
  );
}
