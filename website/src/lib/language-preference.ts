/**
 * 营销站（静态导出）侧的浏览器语言偏好读写。
 *
 * 权威定义在 `frontend/src/lib/language-preference.ts`（控制台 SPA）：两边的
 * 键名与语义必须逐字一致，否则「营销站选语言 → 控制台仍是旧语言」会复发。
 * 这里只保留营销站需要的子集，不引控制台的运行时依赖。
 *
 * 所有读写都吞掉异常：隐私模式或站点数据被清时降级为「无本地记忆」，
 * 语言切换与导航必须照常工作（Review Focus 1）。
 */

import { isLocale, type Locale } from "@/config/locales";

export const LOCALE_STORAGE_KEY = "newtg.language";
export const LANGUAGE_PENDING_KEY = "newtg.language.pending";
export const LANGUAGE_EXPLICIT_KEY = "newtg.language.explicit";
export const LANGUAGE_SCHEMA_KEY = "newtg.language.schema";
export const LANGUAGE_SCHEMA_VERSION = "2";

/** 读取本浏览器缓存的语言；未知值按「没存过」处理。 */
export function readStoredLocale(): Locale | null {
  try {
    const value = localStorage.getItem(LOCALE_STORAGE_KEY);
    return value !== null && isLocale(value) ? value : null;
  } catch {
    return null;
  }
}

/** 只写语言缓存；服务端偏好落地、F5 归一化与营销站守卫都用它。 */
export function writeStoredLocale(locale: Locale): void {
  try {
    localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  } catch {
    // Storage unavailable; the caller still navigates.
  }
}

export function hasPendingSync(): boolean {
  return readFlag(LANGUAGE_PENDING_KEY);
}

export function hasExplicitChoice(): boolean {
  return readFlag(LANGUAGE_EXPLICIT_KEY);
}

/** 仅用户手势调用：语言 + explicit + pending 三件套。 */
export function markLanguageChosen(locale: Locale): void {
  writeStoredLocale(locale);
  writeFlag(LANGUAGE_PENDING_KEY);
  writeFlag(LANGUAGE_EXPLICIT_KEY);
}

/** 回写成功后清掉待同步标记，保留 explicit。 */
export function clearPendingSync(): void {
  try {
    localStorage.removeItem(LANGUAGE_PENDING_KEY);
  } catch {
    // Storage unavailable; nothing to clear.
  }
}

/**
 * 一次性迁移标记：`schema !== "2"` 时写入 "2"，但绝不动 explicit
 * （存量 `newtg.language` 里无法区分显式选择与派生值）。
 */
export function ensureLanguageSchema(): void {
  try {
    if (localStorage.getItem(LANGUAGE_SCHEMA_KEY) !== LANGUAGE_SCHEMA_VERSION) {
      localStorage.setItem(LANGUAGE_SCHEMA_KEY, LANGUAGE_SCHEMA_VERSION);
    }
  } catch {
    // Storage unavailable; the migration marker is best-effort.
  }
}

function readFlag(key: string): boolean {
  try {
    return localStorage.getItem(key) === "1";
  } catch {
    return false;
  }
}

function writeFlag(key: string): void {
  try {
    localStorage.setItem(key, "1");
  } catch {
    // Storage unavailable; the caller still navigates.
  }
}
