export const THEME_STORAGE_KEY = "newtg-theme";

export type Theme = "light" | "dark";

export function resolveTheme(stored: string | null | undefined, prefersDark: boolean): Theme {
  if (stored === "light" || stored === "dark") {
    return stored;
  }
  return prefersDark ? "dark" : "light";
}

export function applyTheme(theme: Theme): void {
  const root = document.documentElement;
  root.dataset.theme = theme;
  root.style.colorScheme = theme;
}

export function storeTheme(theme: Theme): void {
  try {
    localStorage.setItem(THEME_STORAGE_KEY, theme);
  } catch {
    // Storage can be unavailable in private contexts; the in-memory theme still applies.
  }
}

export function readStoredTheme(): Theme | null {
  try {
    const stored = localStorage.getItem(THEME_STORAGE_KEY);
    return stored === "light" || stored === "dark" ? stored : null;
  } catch {
    return null;
  }
}

/**
 * Runs in <head> before first paint so the page never flashes the wrong theme.
 * Follows the system preference unless the visitor has made an explicit choice.
 */
export const THEME_INIT_SCRIPT = `(function(){var t="light";try{if(window.matchMedia&&window.matchMedia("(prefers-color-scheme: dark)").matches){t="dark"}}catch(e){}try{var s=localStorage.getItem("${THEME_STORAGE_KEY}");if(s==="light"||s==="dark"){t=s}}catch(e){}var r=document.documentElement;r.dataset.theme=t;r.style.colorScheme=t})();`;
