import { useCallback, useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'

import { ApiError } from '@/api/errors'
import { fetchReadyz } from '@/features/public/api'
import { sanitizeAuthRedirect } from '@/lib/auth-redirect'
import { meQueryOptions } from '@/lib/session'
import { Button } from '@/shared/ui/button'

import {
  ErrorActionLink,
  ErrorPageShell,
  useErrorPageTranslate,
  useOfflineStatus,
} from './error-page-shell'

/**
 * 五张错误页的差异部分（F-09-09～F-09-13）。公共外壳在 `error-page-shell`，
 * 这里只实现每页的图标色（由 status 决定）、文案键、按钮集与重试策略。
 */

/** 与 `features/public/site-status-card` 共享的 readyz 缓存键。 */
const READYZ_QUERY_KEY = ['public', 'readyz'] as const

/**
 * `site.docs_url` / `site.status_page_url` / `nav.pricing.enabled` 在
 * modules-09 中仍标注为【假设】（后端 `GET /api/status` 尚未下发）。这里做
 * 结构读取而不是把它们写进 `schema.d.ts`：契约一旦落地即可生效，缺字段时
 * 按钮自然隐藏，绝不指向可能不存在的页面（F-09-13 验收 2）。
 */
export interface OptionalSiteConfig {
  docsUrl?: string
  statusPageUrl?: string
  pricingEnabled: boolean
}

export function readOptionalSiteConfig(status: unknown): OptionalSiteConfig {
  const config: OptionalSiteConfig = { pricingEnabled: false }
  if (typeof status !== 'object' || status === null) return config
  const record = status as Record<string, unknown>

  const site = record.site
  if (typeof site === 'object' && site !== null) {
    const docs = (site as Record<string, unknown>).docs_url
    if (typeof docs === 'string' && docs.length > 0) config.docsUrl = docs
    const statusPage = (site as Record<string, unknown>).status_page_url
    if (typeof statusPage === 'string' && statusPage.length > 0) {
      config.statusPageUrl = statusPage
    }
  }

  const nav = record.nav
  if (typeof nav === 'object' && nav !== null) {
    const pricing = (nav as Record<string, unknown>).pricing
    if (typeof pricing === 'object' && pricing !== null) {
      config.pricingEnabled =
        (pricing as Record<string, unknown>).enabled === true
    }
  }

  return config
}

/**
 * fetch 传输失败（DNS/连接拒绝等）在 `parseProblem` 里归一为 `status: 0` /
 * `kind: 'offline'`。浏览器此时 `navigator.onLine` 仍可能为 true，所以探测
 * 错误必须显式传给骨架，否则会把断网误报成权限或维护状态（web-00 §8.5）。
 */
function isTransportFailure(error: unknown): boolean {
  return error instanceof ApiError && error.kind === 'offline'
}

export interface ErrorStatusPageProps {
  status: 401 | 403
  /** 触发 401/403 的原路径；经 `sanitizeAuthRedirect` 后才能进入登录链接。 */
  redirect?: string
  requestId?: string
  /** 403 停留期间探测会话；关闭后仅渲染静态页（测试/离线场景）。 */
  probeSession?: boolean
  /** 403 探测到会话失效时的落点回调（由路由注入 router.navigate）。 */
  onSessionExpired?: (redirectTo: string) => void
  /**
   * 403 是否展示「返回上一页」。默认以 `history.length > 1` 近似「有可回退
   * 历史」——浏览器不提供更精确的判定，错误页也不值得为此引入导航栈状态。
   */
  canGoBack?: boolean
}

function currentPath(): string {
  if (typeof location === 'undefined') return '/'
  return `${location.pathname}${location.search}`
}

export function ErrorStatusPage({
  status,
  redirect,
  requestId,
  probeSession = true,
  onSessionExpired,
  canGoBack,
}: ErrorStatusPageProps) {
  const t = useErrorPageTranslate()
  const offline = useOfflineStatus()

  /**
   * 403 只服务「已登录但无权限」（modules-09 A-2 裁决）。会话在停留期间
   * 失效（/me → 401）时必须转去登录，不能把用户留在 403；探测失败但不是
   * 401（服务故障/离线）时保守渲染本页，且不展示任何身份断言（F-09-10）。
   */
  const probe = useQuery({
    ...meQueryOptions(),
    enabled: status === 403 && probeSession && !offline,
    // 停留期间必须持续观察会话：用户可能在别的标签页退出/被踢，只在挂载时
    // 探测一次会让 403 页一直留在屏幕上（F-09-10 TC-05）。30s 与 503 的
    // readyz 探测同频；refetchOnWindowFocus 仍保留默认的立即复查。
    refetchInterval: 30_000,
  })

  useEffect(() => {
    if (status !== 403) return
    const error = probe.error
    if (!(error instanceof ApiError) || error.kind !== 'auth') return
    onSessionExpired?.(sanitizeAuthRedirect(redirect ?? currentPath()))
  }, [status, probe.error, onSessionExpired, redirect])

  if (status === 401) {
    // 无 return_to 时直接去 /sign-in（默认跳 /dashboard）；有则保留经清洗
    // 的站内路径。外部域 / `//evil` 由 sanitizeAuthRedirect 统一重置。
    const signInHref =
      redirect !== undefined && redirect !== ''
        ? `/sign-in?redirect=${encodeURIComponent(sanitizeAuthRedirect(redirect))}`
        : '/sign-in'

    return (
      <ErrorPageShell
        status={401}
        title={t('common.error_page.401.title')}
        body={t('common.error_page.401.body')}
        requestId={requestId}
        networkFailure={false}
        actions={
          <>
            <ErrorActionLink href={signInHref}>
              {t('common.error_page.401.sign_in')}
            </ErrorActionLink>
            <ErrorActionLink href="/" variant="secondary">
              {t('common.error_page.home')}
            </ErrorActionLink>
          </>
        }
      />
    )
  }

  const hasHistory =
    canGoBack ?? (typeof window !== 'undefined' && window.history.length > 1)

  return (
    <ErrorPageShell
      status={403}
      title={t('common.error_page.403.title')}
      body={t('common.error_page.403.body')}
      requestId={requestId}
      networkFailure={isTransportFailure(probe.error)}
      actions={
        <>
          <ErrorActionLink href="/">{t('common.error_page.home')}</ErrorActionLink>
          {hasHistory ? (
            <Button
              type="button"
              variant="secondary"
              className="h-11 w-full px-6 sm:w-auto"
              onClick={() => {
                window.history.back()
              }}
            >
              {t('common.error_page.go_back')}
            </Button>
          ) : (
            <ErrorActionLink href="/dashboard" variant="secondary">
              {t('common.error_page.console')}
            </ErrorActionLink>
          )}
        </>
      }
    />
  )
}

export interface ServerErrorPageProps {
  requestId?: string
  /** 可重发的原请求；缺省时隐藏「重试」（F-09-12 按钮表）。 */
  onRetry?: () => void | Promise<void>
  /** 由 `ApiError.kind === 'offline'` 传入，与 navigator 离线信号合并。 */
  networkFailure?: boolean
  /** 重试是否可用；默认以 `onRetry` 是否存在判定。 */
  retryable?: boolean
}

export function ServerErrorPage({
  requestId,
  onRetry,
  networkFailure = false,
  retryable,
}: ServerErrorPageProps) {
  const t = useErrorPageTranslate()
  const onlineOffline = useOfflineStatus()
  // fetch 网络错误与 navigator 离线信号任一成立都视为离线：恢复前重试不可用。
  const offline = onlineOffline || networkFailure
  const [retrying, setRetrying] = useState(false)
  const canRetry = (retryable ?? onRetry !== undefined) && onRetry !== undefined

  const retry = useCallback(async () => {
    if (onRetry === undefined || retrying) return
    setRetrying(true)
    try {
      // 重试失败时错误会再次冒泡到边界并带来新的 request_id（F-09-12 异常流程）；
      // 这里只负责 loading/禁用，避免并发重复提交。
      await onRetry()
    } finally {
      setRetrying(false)
    }
  }, [onRetry, retrying])

  return (
    <ErrorPageShell
      status={500}
      title={t('common.error_page.500.title')}
      body={t('common.error_page.500.body')}
      requestId={requestId}
      networkFailure={networkFailure}
      actions={
        <>
          {canRetry ? (
            <Button
              type="button"
              className="h-11 w-full px-6 sm:w-auto"
              onClick={() => void retry()}
              disabled={offline || retrying}
              aria-busy={retrying}
            >
              {retrying ? (
                <span
                  aria-hidden="true"
                  className="size-4 animate-spin rounded-full border-2 border-current border-t-transparent"
                />
              ) : null}
              {t('common.action.retry')}
            </Button>
          ) : null}
          <ErrorActionLink href="/" variant="secondary">
            {t('common.error_page.home')}
          </ErrorActionLink>
        </>
      }
    />
  )
}

/**
 * 503 探测间隔（F-09-13 超限态）：默认 30s；探测本身被限流（429）时拉长到
 * 60s，避免在服务恢复前继续加压。导出为纯函数以便直接验证 429 分支。
 */
export function readyzProbeInterval(error: unknown, baseMs: number): number {
  return error instanceof ApiError && error.status === 429 ? 60_000 : baseMs
}

export interface ServiceUnavailablePageProps {
  requestId?: string
  /** 可重发的原请求；无 redirect 时路由层传入整页 reload。 */
  onRetry?: () => void | Promise<void>
  /** `site.status_page_url`；为空时不渲染状态页按钮。 */
  statusPageUrl?: string
  /** 恢复探测间隔；modules-09 冻结 30s，测试可缩短。 */
  probeIntervalMs?: number
}

export function ServiceUnavailablePage({
  requestId,
  onRetry,
  statusPageUrl,
  probeIntervalMs = 30_000,
}: ServiceUnavailablePageProps) {
  const t = useErrorPageTranslate()
  const offline = useOfflineStatus()
  const [retrying, setRetrying] = useState(false)

  /**
   * F-09-13 自动恢复：每 30s 静默探测 `GET /readyz`。探测失败不改 UI
   * （避免闪烁）；429 时拉长到 60s（超限态）。`status === 'ok'` 才算恢复，
   * degraded 仍然是「没恢复」。
   */
  const probe = useQuery({
    queryKey: READYZ_QUERY_KEY,
    queryFn: ({ signal }) => fetchReadyz(signal),
    retry: false,
    refetchInterval: (query) =>
      readyzProbeInterval(query.state.error, probeIntervalMs),
    refetchIntervalInBackground: false,
  })

  const recovered = probe.data?.status === 'ok'

  // 恢复在线时立即探测一次（F-09-13 离线态）。用上一次的离线值判断方向，
  // 避免挂载时（本来就在线）多打一条与首次查询重复的请求。
  const wasOffline = useRef(offline)
  useEffect(() => {
    const cameBack = wasOffline.current && !offline
    wasOffline.current = offline
    if (cameBack) void probe.refetch()
    // 仅在在线状态翻转时探测；refetch 是稳定引用。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [offline])

  const retry = async (): Promise<void> => {
    if (onRetry === undefined || retrying) return
    setRetrying(true)
    try {
      await onRetry()
    } finally {
      setRetrying(false)
    }
  }

  return (
    <ErrorPageShell
      status={503}
      title={t('common.error_page.503.title')}
      body={t('common.error_page.503.body')}
      requestId={requestId}
      networkFailure={isTransportFailure(probe.error)}
      banner={
        recovered ? (
          <div
            role="status"
            aria-live="polite"
            className="mb-6 flex items-center justify-center gap-2 rounded-lg border border-success-line bg-success-bg px-4 py-3 text-sm text-success"
          >
            <svg
              aria-hidden="true"
              viewBox="0 0 16 16"
              className="size-4"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            >
              <path d="m3 8 3.5 3.5L13 5" />
            </svg>
            {t('common.error_page.503.recovered')}
            {/* 恢复横幅内嵌重试（F-09-13 线框）：与下方主按钮同一动作，
                用户看到「已恢复」时就地重发，不必再移动视线。 */}
            {onRetry !== undefined ? (
              <Button
                type="button"
                variant="secondary"
                className="h-8 px-3 text-xs"
                onClick={() => void retry()}
                disabled={offline || retrying}
                aria-busy={retrying}
              >
                {t('common.action.retry')}
              </Button>
            ) : null}
          </div>
        ) : null
      }
      actions={
        <>
          {onRetry !== undefined ? (
            <Button
              type="button"
              className="h-11 w-full px-6 sm:w-auto"
              onClick={() => void retry()}
              disabled={offline || retrying}
              aria-busy={retrying}
            >
              {retrying ? (
                <span
                  aria-hidden="true"
                  className="size-4 animate-spin rounded-full border-2 border-current border-t-transparent"
                />
              ) : null}
              {t('common.action.retry')}
            </Button>
          ) : null}
          {statusPageUrl !== undefined ? (
            <ErrorActionLink
              href={statusPageUrl}
              variant="secondary"
              external
            >
              {t('common.error_page.503.status_page')}
            </ErrorActionLink>
          ) : null}
          <ErrorActionLink href="/" variant="secondary">
            {t('common.error_page.home')}
          </ErrorActionLink>
        </>
      }
    />
  )
}
