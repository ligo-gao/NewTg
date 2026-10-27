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

/**
 * 最近一次 `GET /api/self` 拿到的乐观锁令牌。
 *
 * 为什么缓存：营销站切语言的顺序是「发同步 → location.assign」，而 PUT 必须先
 * 拿到 `updated_at`。若令牌要靠一次网络往返现取，导航提交后 JS 上下文已被销毁，
 * 续体永远不会执行——PUT 从未发出，只剩 pending。缓存由守卫的挂载探测自然预热，
 * 切换时就能在第一个 await 之前把 keepalive 的 PUT 发出去。
 */
let cachedUpdatedAt: string | undefined;

/** 测试与显式登出用：清空模块级令牌缓存（401 也会自动清）。 */
export function resetAccountLocaleCache(): void {
  cachedUpdatedAt = undefined;
}

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
    if (response.status === 401) {
      cachedUpdatedAt = undefined;
      return { status: "anonymous" };
    }
    if (!response.ok) return { status: "failed" };

    const body = (await response.json()) as { language?: unknown; updated_at?: unknown };
    const language = body.language;
    const updatedAt = body.updated_at;
    if (typeof language !== "string" || !isLocale(language) || typeof updatedAt !== "string") {
      return { status: "failed" };
    }
    cachedUpdatedAt = updatedAt;
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
 *
 * 快路径：缓存里有 `updated_at` 且 CSRF Cookie 可读时，本函数在第一次 await
 * 之前就发出 PUT——调用方随后 `location.assign` 也不会掐掉已发出的请求。
 * 没有缓存令牌时才回退到「先探测、再回写」。
 */
export async function syncLanguageToAccount(
  locale: Locale,
  options: { signal?: AbortSignal } = {},
): Promise<SyncResult> {
  try {
    const signal = options.signal;
    const cached = cachedUpdatedAt;
    if (cached !== undefined && readCsrfCookie() !== undefined) {
      return await putWithRetry(locale, cached, signal);
    }

    const account = await fetchAccountLocale(signal ? { signal } : undefined);
    if (account.status === "anonymous") return "anonymous";
    if (account.status === "failed") return "failed";

    return await putWithRetry(locale, account.updated_at, signal);
  } catch {
    return "failed";
  }
}

/**
 * PUT → 409（乐观锁令牌过期，见 `handlePutSelfSetting`）时重探一次、
 * 重试一次；无论结果如何都不再循环。成功即清 pending。
 */
async function putWithRetry(
  locale: Locale,
  updatedAt: string,
  signal: AbortSignal | undefined,
): Promise<SyncResult> {
  let response = await sendPut(locale, updatedAt, signal);

  if (response.status === 409) {
    const account = await fetchAccountLocale(signal ? { signal } : undefined);
    if (account.status === "anonymous") return "anonymous";
    if (account.status === "failed") return "failed";
    response = await sendPut(locale, account.updated_at, signal);
  }

  if (!response.ok) return "failed";
  clearPendingSync();
  // 成功后服务端换了新的 updated_at；不阻塞结果。
  void rememberUpdatedAt(response);
  return "saved";
}

/**
 * 发出一次写请求。CSRF Cookie 可读时 fetch 在同步段调用（见上）；缺失时先
 * `GET /api/me` 让服务端重发，再写。403 `csrf_failed` 冻结重试一次。
 */
async function sendPut(
  locale: Locale,
  updatedAt: string,
  signal: AbortSignal | undefined,
): Promise<Response> {
  const body = JSON.stringify({ language: locale, updated_at: updatedAt });

  let response: Response;
  const token = readCsrfCookie();
  if (token !== undefined) {
    response = await putLanguage(body, token, signal);
  } else {
    await refreshCsrfCookie();
    response = await putLanguage(body, readCsrfCookie(), signal);
  }

  if (await isCsrfFailure(response)) {
    // CSRF Cookie 被轮换：先让服务端重发，再按冻结规则重试一次。
    await refreshCsrfCookie();
    response = await putLanguage(body, readCsrfCookie(), signal);
  }
  return response;
}

/** 用 PUT 响应里的新令牌更新缓存；解析失败不影响调用方。 */
async function rememberUpdatedAt(response: Response): Promise<void> {
  try {
    const body = (await response.clone().json()) as { updated_at?: unknown };
    if (typeof body.updated_at === "string") cachedUpdatedAt = body.updated_at;
  } catch {
    // Response body is advisory here; the next probe refreshes it anyway.
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
