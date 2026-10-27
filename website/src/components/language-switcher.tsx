"use client";

import type { ChangeEvent } from "react";
import { LOCALES, localeInfo, localePath, type Locale } from "@/config/locales";
import {
  ensureLanguageSchema,
  markLanguageChosen,
} from "@/lib/language-preference";
import { syncLanguageToAccount } from "@/lib/language-sync";

interface LanguageSwitcherProps {
  locale: Locale;
  label: string;
  className?: string;
  /** Show only the short code in narrow headers; the native menu keeps full names. */
  compact?: boolean;
}

/**
 * Native <select> keeps the control keyboard/screen-reader accessible and
 * matches the console's language picker. Changing it navigates to the
 * locale's static route (/en/, /fr/, …); '/' is Simplified Chinese.
 *
 * A pick is a user gesture: it writes the console's shared preference keys
 * (language + explicit + pending) and starts a `keepalive` write to
 * `PUT /api/self/setting` in the background, so the choice survives the
 * navigation and reaches the account. A failed sync keeps `pending` for the
 * console (or the next marketing page load) to retry; it never blocks.
 */
export function LanguageSwitcher({
  locale,
  label,
  className = "",
  compact = false,
}: LanguageSwitcherProps) {
  function handleChange(event: ChangeEvent<HTMLSelectElement>) {
    const next = event.target.value as Locale;
    ensureLanguageSchema();
    markLanguageChosen(next);
    // 后台回写账号偏好；keepalive 让请求活过下面的整页导航。
    void syncLanguageToAccount(next);

    const currentPath = window.location.pathname;
    const currentLocale = localeInfo(locale);
    const nextBase = localePath(next);

    // 移除当前语言前缀，获取页面路径
    let pagePath = currentPath;
    if (currentLocale.path !== "/") {
      // 移除当前语言路径前缀（例如 /en/, /fr/）
      pagePath = currentPath.replace(new RegExp(`^${currentLocale.path.replace(/\/$/, '')}`), '');
    }

    // 确保路径以 / 开头
    if (!pagePath.startsWith('/')) {
      pagePath = '/' + pagePath;
    }

    // 构建目标路径：新语言前缀 + 页面路径
    let destination: string;
    if (nextBase === "/") {
      // 简体中文（默认语言）直接使用页面路径
      destination = pagePath === "/" ? "/" : pagePath;
    } else {
      // 其他语言：语言前缀 + 页面路径
      destination = pagePath === "/"
        ? nextBase
        : `${nextBase.replace(/\/$/, '')}${pagePath}`;
    }

    window.location.assign(destination);
  }

  const current = localeInfo(locale);

  if (compact) {
    return (
      <label
        className={`relative inline-flex h-9 w-[52px] items-center justify-center gap-1 rounded-md border border-line-2 bg-transparent text-[12px] font-medium text-muted transition-colors hover:border-cyan-dim hover:text-ink ${className}`}
      >
        <span aria-hidden="true">{current.shortLabel}</span>
        <select
          value={locale}
          onChange={handleChange}
          aria-label={label}
          className="absolute inset-0 size-full cursor-pointer opacity-0"
        >
          {LOCALES.map((item) => (
            <option key={item.code} value={item.code}>
              {item.endonym}
            </option>
          ))}
        </select>
      </label>
    );
  }

  return (
    <label
      className={`relative inline-flex h-9 items-center gap-1.5 rounded-md border border-line-2 bg-transparent pl-2 pr-6 text-[12.5px] text-muted transition-colors hover:border-cyan-dim hover:text-ink ${className}`}
    >
      <span className="sr-only">{label}</span>
      <svg
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.7"
        strokeLinecap="round"
        aria-hidden="true"
        className="size-3.5 flex-none"
      >
        <circle cx="12" cy="12" r="9" />
        <path d="M3 12h18M12 3c2.5 2.4 4 5.6 4 9s-1.5 6.6-4 9c-2.5-2.4-4-5.6-4-9s1.5-6.6 4-9Z" />
      </svg>
      <select
        value={locale}
        onChange={handleChange}
        aria-label={label}
        className="max-w-[110px] cursor-pointer appearance-none truncate bg-transparent pr-0 text-[12.5px] text-inherit outline-none"
      >
        {LOCALES.map((item) => (
          <option key={item.code} value={item.code} className="bg-panel text-ink">
            {item.endonym}
          </option>
        ))}
      </select>
      <svg
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
        className="pointer-events-none absolute right-2 size-3"
      >
        <path d="m6 9 6 6 6-6" />
      </svg>
    </label>
  );
}
