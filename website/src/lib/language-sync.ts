/**
 * 营销站的后台同步：把浏览器的语言选择写回账号（`PUT /api/self/setting`），
 * 以及探测账号当前语言（`GET /api/self`）。
 *
 * 安全面与控制台 `frontend/src/lib/csrf.ts` 保持一致：Cookie 会话的写请求必须
 * 带 `__Host-nkt_csrf`，缺失时先 `GET /api/me` 让服务端重发；403
 * `csrf_failed` 只重试一次。这里只复制最小常量，权威定义仍在控制台。
 *
 * 营销站是 Cookie 会话 + 同源部署；本地 dev 跨端口时会失败并保留 pending，
 * 降级为「仅本浏览器一致」。**任何路径都不抛异常**——导航已经发生。
 */

import { isLocale, type Locale } from "@/config/locales";
import { clearPendingSync } from "./language-preference";

const ACCOUNT_PATH = "/api/self";
const SETTING_PATH = "/api/self/setting";
const CSRF_COOKIE = "__Host-nkt_csrf";
const CSRF_HEADER = "X-CSRF-Token";
const CSRF_REFRESH_PATH = "/api/me";
const DEFAULT_TIMEOUT_MS = 1500;

export type AccountLocaleResult =
  | { status: "ok"; language: Locale; updated_at: string }
  | { status: "anonymous" }
  | { status: "failed" };

export type SyncResult = "saved" | "anonymous" | "failed";

export interface AccountLocaleOptions {
  timeoutMs?: number;
  signal?: AbortSignal;
}

/**
 * 探测账号语言。401 是匿名（Review Focus 2），不是错误；超时、网络失败、
 * 非 2xx、以及非法语言值一律 `failed`，调用方据此保持当前 URL。
 *
 * `updated_at` 随结果一起返回：它是 `PUT /api/self/setting` 乐观锁的令牌，
 * 与 language 来自同一个响应，避免同步时再发一次 GET。
 */
export async function fetchAccountLocale(
  options: AccountLocaleOptions = {},
): Promise<AccountLocaleResult> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), options.timeoutMs ?? DEFAULT_TIMEOUT_MS);
  const onExternalAbort = () => controller.abort();
  if (options.signal) {
    if (options.signal.aborted) controller.abort();
    else options.signal.addEventListener("abort", onExternalAbort, { once: true });
  }

  try {
    const response = await fetch(ACCOUNT_PATH, {
      credentials: "same-origin",
      cache: "no-store",
      headers: { Accept: "application/json" },
      signal: controller.signal,
    });
    if (response.status === 401) return { status: "anonymous" };
    if (!response.ok) return { status: "failed" };

    const body = (await response.json()) as { language?: unknown; updated_at?: unknown };
    const language = body.language;
    const updatedAt = body.updated_at;
    if (typeof language !== "string" || !isLocale(language) || typeof updatedAt !== "string") {
      return { status: "failed" };
    }
    return { status: "ok", language, updated_at: updatedAt };
  } catch {
    return { status: "failed" };
  } finally {
    clearTimeout(timer);
    options.signal?.removeEventListener("abort", onExternalAbort);
  }
}

/**
 * 把一次显式选择写回账号。成功清掉 pending；匿名或失败都保留 pending，
 * 由控制台或下一次营销站加载补写。`keepalive` 让请求活过紧接着的导航。
 */
export async function syncLanguageToAccount(
  locale: Locale,
  options: { signal?: AbortSignal } = {},
): Promise<SyncResult> {
  try {
    const account = await fetchAccountLocale(
      options.signal ? { signal: options.signal } : undefined,
    );
    if (account.status === "anonymous") return "anonymous";
    if (account.status === "failed") return "failed";

    const body = JSON.stringify({ language: locale, updated_at: account.updated_at });

    let response = await putLanguage(body, await readCsrfToken(), options.signal);
    if (await isCsrfFailure(response)) {
      // CSRF Cookie 被轮换：先让服务端重发，再按冻结规则重试一次。
      await refreshCsrfCookie();
      response = await putLanguage(body, await readCsrfToken(), options.signal);
    }
    if (!response.ok) return "failed";

    clearPendingSync();
    return "saved";
  } catch {
    return "failed";
  }
}

function putLanguage(
  body: string,
  token: string | undefined,
  signal: AbortSignal | undefined,
): Promise<Response> {
  const headers = new Headers({ "Content-Type": "application/json" });
  if (token !== undefined) headers.set(CSRF_HEADER, token);
  return fetch(SETTING_PATH, {
    method: "PUT",
    credentials: "same-origin",
    keepalive: true,
    headers,
    body,
    signal,
  });
}

/** Reads the non-HttpOnly CSRF cookie; refreshes via `/api/me` when missing. */
async function readCsrfToken(): Promise<string | undefined> {
  const existing = readCsrfCookie();
  if (existing !== undefined) return existing;
  await refreshCsrfCookie();
  return readCsrfCookie();
}

function readCsrfCookie(): string | undefined {
  if (typeof document === "undefined") return undefined;
  const prefix = `${CSRF_COOKIE}=`;
  const entry = document.cookie
    .split(";")
    .map((part) => part.trim())
    .find((part) => part.startsWith(prefix));
  if (entry === undefined) return undefined;
  const raw = entry.slice(prefix.length);
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

async function refreshCsrfCookie(): Promise<void> {
  try {
    await fetch(CSRF_REFRESH_PATH, {
      method: "GET",
      credentials: "same-origin",
      cache: "no-store",
      headers: { Accept: "application/json" },
    });
  } catch {
    // Offline or transient failure: the retry decides the outcome.
  }
}

async function isCsrfFailure(response: Response): Promise<boolean> {
  if (response.status !== 403) return false;
  try {
    const body: unknown = await response.clone().json();
    if (typeof body !== "object" || body === null) return false;
    const type = (body as Record<string, unknown>).type;
    return typeof type === "string" && type.endsWith("csrf_failed");
  } catch {
    return false;
  }
}
