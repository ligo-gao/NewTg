"use client";

import { useEffect } from "react";
import { usePathname } from "next/navigation";
import { localeInfo } from "@/config/locales";
import {
  hasExplicitChoice,
  hasPendingSync,
  readStoredLocale,
  writeStoredLocale,
} from "@/lib/language-preference";
import { fetchAccountLocale, syncLanguageToAccount } from "@/lib/language-sync";
import { localeForPath, localizePath } from "@/lib/locale-path";

const PROBE_TIMEOUT_MS = 1500;

/**
 * 营销站的客户端守卫（Global Language Selection §3.3）：
 *
 * 1. `pending` 存在 → 本地选择胜出，跳到对应语言路由，并借 keepalive 补写一次；
 * 2. 否则探测 `GET /api/self`：已登录以账号语言为准（只写 `newtg.language`）；
 * 3. 匿名且显式选过 → 按本地选择跳转；从没选过 → 保持当前 URL（分享链接不被劫持）；
 * 4. 失败/超时 → 保持当前 URL，永不阻塞渲染。
 *
 * 顺带按路径设置 `<html lang>`：静态导出无法在构建期按 URL 改根元素。
 */
export function LocalePreferenceRoute() {
  const pathname = usePathname() ?? "";

  useEffect(() => {
    const locale = localeForPath(pathname);
    if (locale === null) return;
    document.documentElement.lang = localeInfo(locale).htmlLang;
  }, [pathname]);

  useEffect(() => {
    const current = localeForPath(pathname);
    if (current === null) return;

    let cancelled = false;

    async function decide() {
      if (hasPendingSync()) {
        const local = readStoredLocale();
        if (local === null) return;
        // 即使当前 URL 已是本地选择也要补写一次：此刻没有跳转，上下文还活着，
        // 探测 + PUT 能真正完成并清掉 pending。只有语言不同才需要导航。
        void syncLanguageToAccount(local);
        if (local !== current) {
          window.location.assign(localizePath(pathname, local));
        }
        return;
      }

      const account = await fetchAccountLocale({ timeoutMs: PROBE_TIMEOUT_MS });
      if (cancelled) return;

      if (account.status === "ok") {
        // 服务端偏好落地只写语言值，不写 explicit / pending。
        writeStoredLocale(account.language);
        if (account.language !== current) {
          window.location.assign(localizePath(pathname, account.language));
        }
        return;
      }

      if (account.status === "anonymous" && hasExplicitChoice()) {
        const local = readStoredLocale();
        if (local !== null && local !== current) {
          window.location.assign(localizePath(pathname, local));
        }
      }
    }

    void decide();

    return () => {
      cancelled = true;
    };
  }, [pathname]);

  return null;
}
