import { beforeEach, describe, expect, it } from "vitest";

import { LOCALES, isLocale } from "@/config/locales";

import {
  LANGUAGE_EXPLICIT_KEY,
  LANGUAGE_PENDING_KEY,
  LANGUAGE_SCHEMA_KEY,
  LANGUAGE_SCHEMA_VERSION,
  LOCALE_STORAGE_KEY,
  ensureLanguageSchema,
  hasExplicitChoice,
  hasPendingSync,
  markLanguageChosen,
  readStoredLocale,
} from "./language-preference";

const ACCEPTED_LOCALES = ["en", "zh-CN", "zh-TW", "fr", "ru", "ja", "vi"];

beforeEach(() => localStorage.clear());

/**
 * 跨应用存储契约。控制台在 `frontend/src/lib/language-preference.ts` 里
 * 硬编码同一组键；任一侧改名或升级 schema 都不会有编译错误，症状恰好是
 * 「营销站选完语言，控制台又切回去」。这里钉住字面量，而不是实现细节。
 */
describe("storage contract with the console (frontend/)", () => {
  it("pins the four storage keys and the schema version", () => {
    expect(LOCALE_STORAGE_KEY).toBe("newtg.language");
    expect(LANGUAGE_PENDING_KEY).toBe("newtg.language.pending");
    expect(LANGUAGE_EXPLICIT_KEY).toBe("newtg.language.explicit");
    expect(LANGUAGE_SCHEMA_KEY).toBe("newtg.language.schema");
    expect(LANGUAGE_SCHEMA_VERSION).toBe("2");
  });

  it("accepts exactly the seven bundled locales", () => {
    for (const locale of ACCEPTED_LOCALES) {
      expect(isLocale(locale)).toBe(true);
    }
    expect(LOCALES.map((entry) => entry.code)).toEqual(ACCEPTED_LOCALES);
    for (const other of ["de", "en-US", "zh-cn", ""]) {
      expect(isLocale(other)).toBe(false);
    }
  });

  it("leaves a gesture readable by all three marker readers", () => {
    markLanguageChosen("vi");
    expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBe("vi");
    expect(localStorage.getItem(LANGUAGE_EXPLICIT_KEY)).toBe("1");
    expect(localStorage.getItem(LANGUAGE_PENDING_KEY)).toBe("1");
    expect(readStoredLocale()).toBe("vi");
    expect(hasExplicitChoice()).toBe(true);
    expect(hasPendingSync()).toBe(true);
  });

  it("stamps the schema version without fabricating explicit or pending", () => {
    ensureLanguageSchema();
    expect(localStorage.getItem(LANGUAGE_SCHEMA_KEY)).toBe("2");
    expect(localStorage.getItem(LANGUAGE_EXPLICIT_KEY)).toBeNull();
    expect(localStorage.getItem(LANGUAGE_PENDING_KEY)).toBeNull();
    expect(hasExplicitChoice()).toBe(false);
    expect(hasPendingSync()).toBe(false);
    expect(readStoredLocale()).toBeNull();
  });
});
