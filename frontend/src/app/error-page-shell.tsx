import {
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from 'react'

import { translate, getActiveLocale, type TranslateFn } from '@/app/providers/i18n'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { useToastStore } from '@/shared/ui/toast'

/**
 * F-09-08 错误页公共骨架（web-00 §8.3）。
 *
 * 五张错误页只写差异（图标 / 状态码 / 文案 / 按钮集），骨架本身统一负责：
 * 居中 560px 列、大号状态码、标题层级、request_id 芯片与复制、离线变体。
 *
 * 为什么图形用内联 SVG 而不引入图标运行时：错误页本身就是「应用已经坏了」
 * 的终点，不能再把它绑定到额外的组件/数据依赖；同时 00 §3.4 的首屏 JS
 * 预算（150 KiB gzip）要求零体积图形。图形全部 `aria-hidden`，语义由 h1
 * 承载（UI §8 可访问性）。
 */

export type ErrorStatus = 401 | 403 | 404 | 500 | 503

/**
 * 读取在线状态。`navigator.onLine === false` 是浏览器给出的唯一可靠离线
 * 信号（web-00 §8.5）；fetch 层网络错误由调用方的 `networkFailure` 传入。
 */
export function useOfflineStatus(): boolean {
  const [offline, setOffline] = useState<boolean>(
    () => typeof navigator !== 'undefined' && navigator.onLine === false,
  )

  useEffect(() => {
    const update = (): void => {
      setOffline(navigator.onLine === false)
    }
    // 初始值可能与事件之间发生变化（例如组件挂载前已断网），补一次同步。
    update()
    window.addEventListener('online', update)
    window.addEventListener('offline', update)
    return () => {
      window.removeEventListener('online', update)
      window.removeEventListener('offline', update)
    }
  }, [])

  return offline
}

/**
 * 错误页可能在 `AppProviders` 之外渲染（全局 React ErrorBoundary 位于
 * provider 外层，见 main.tsx）。这种情况下 `useI18n` 会抛错；错误页自己
 * 不能再抛错，否则兜底页白屏。退回模块级 `translate`（common 是常驻基线），
 * 即便词典尚未加载也只会显示键名，不会显示硬编码英文。
 */
export function useErrorPageTranslate(): TranslateFn {
  try {
    return useI18n().t
  } catch {
    return (key, values) => translate(getActiveLocale(), key, values)
  }
}

const STATUS_ICON_PATHS: Record<ErrorStatus, ReactNode> = {
  // lock
  401: (
    <>
      <rect x="5" y="11" width="14" height="9" rx="2" />
      <path d="M8 11V8a4 4 0 0 1 8 0v3" />
    </>
  ),
  // shield-off
  403: (
    <>
      <path d="M12 3 4.5 6v5.5c0 4.2 3 7.6 7.5 9 1.2-.4 2.3-1 3.2-1.8" />
      <path d="m4 20 16-16" />
    </>
  ),
  // search-x
  404: (
    <>
      <circle cx="10.5" cy="10.5" r="6.5" />
      <path d="m15.5 15.5 4 4" />
      <path d="m8 8 5 5M13 8l-5 5" />
    </>
  ),
  // triangle-alert
  500: (
    <>
      <path d="M12 4 3 20h18L12 4Z" />
      <path d="M12 10v4M12 17.5v.5" />
    </>
  ),
  // wrench
  503: (
    <path d="M15.5 4a5 5 0 0 0-4.4 7.4L4 18.5 5.5 20l7.1-7.1A5 5 0 0 0 20 8.5L17 11l-2.5-2.5L17 6l-1.5-2Z" />
  ),
}

const ICON_TONE: Record<ErrorStatus, string> = {
  401: 'text-neutral',
  403: 'text-danger',
  404: 'text-ink-3',
  500: 'text-danger',
  503: 'text-warning',
}

function StatusIcon({ status }: { status: ErrorStatus }) {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 24 24"
      className={['mx-auto size-6', ICON_TONE[status]].join(' ')}
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      {STATUS_ICON_PATHS[status]}
    </svg>
  )
}

/** 与 `shared/ui/base/button` 同一套 token 的手动样式，供 `<a>` 复用。 */
const ACTION_BASE =
  'inline-flex h-11 w-full items-center justify-center gap-1.5 rounded-lg border border-transparent px-6 text-sm font-medium transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50 sm:w-auto'

const ACTION_VARIANT = {
  primary: 'bg-brand text-on-brand hover:bg-brand-hover active:bg-brand-active',
  secondary:
    'border-border-default bg-surface-card text-ink-1 hover:bg-surface-hover',
} as const

export function errorActionClassName(
  variant: keyof typeof ACTION_VARIANT = 'primary',
): string {
  return `${ACTION_BASE} ${ACTION_VARIANT[variant]}`
}

export interface ErrorActionLinkProps {
  href: string
  variant?: keyof typeof ACTION_VARIANT
  /** 新标签打开（文档 / 状态页）。 */
  external?: boolean
  children: ReactNode
}

/**
 * 错误页按钮用真实 `<a href>`（UI §9 可访问性）：错误页处于路由/会话已经
 * 失效的语境，整页导航比 SPA 跳转更能把 router 与 query 缓存恢复到干净
 * 状态；同时浏览器原生链接支持中键/新标签与读屏器。
 */
export function ErrorActionLink({
  href,
  variant = 'primary',
  external = false,
  children,
}: ErrorActionLinkProps) {
  const externalProps = external
    ? { target: '_blank', rel: 'noopener noreferrer' }
    : {}
  return (
    <a href={href} className={errorActionClassName(variant)} {...externalProps}>
      {children}
    </a>
  )
}

interface RequestIdRowProps {
  requestId: string
  t: TranslateFn
}

/**
 * request_id 芯片（web-00 §8.4）：只在真实值存在时渲染，不显示「—」占位。
 * 复制失败时降级为手动选择 —— 输入框本身可选中，且不会把 request_id 写进
 * URL 或埋点（§8.4 脱敏红线）。
 */
function RequestIdRow({ requestId, t }: RequestIdRowProps) {
  const [status, setStatus] = useState<'idle' | 'copied' | 'failed'>('idle')
  const inputRef = useRef<HTMLInputElement>(null)
  const push = useToastStore((state) => state.push)

  const copy = async (): Promise<void> => {
    let copied = false
    try {
      if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(requestId)
        copied = true
      }
    } catch {
      copied = false
    }

    if (copied) {
      setStatus('copied')
      push({ tone: 'success', message: t('common.action.copied') })
      return
    }

    // 剪贴板不可用：选中芯片文本，用户可直接 Ctrl/Cmd+C。
    setStatus('failed')
    inputRef.current?.focus()
    inputRef.current?.select()
    push({ tone: 'error', message: t('common.error_page.copy_failed') })
  }

  return (
    <div className="mt-6">
      <div className="flex items-center gap-2 rounded-lg bg-surface-subtle px-3 py-2">
        <span className="shrink-0 text-xs text-ink-3">
          {t('common.error_page.request_id_label')}
        </span>
        <input
          ref={inputRef}
          readOnly
          value={requestId}
          aria-label={t('common.error_page.request_id_label')}
          className="min-w-0 flex-1 bg-transparent font-mono text-[13px] leading-5 tabular-nums text-ink-2 outline-none"
        />
        <Button
          type="button"
          variant="secondary"
          onClick={() => void copy()}
          aria-label={t('common.error_page.copy_request_id')}
          className="h-8 shrink-0 px-2 text-xs"
        >
          {t('common.action.copy')}
        </Button>
      </div>
      {/*
        aria-live 让复制结果可被读屏器感知（F-09-08 可访问性）；
        视觉反馈由 Toast 承担，这里保持视觉隐藏以免重复。
      */}
      <p role="status" aria-live="polite" className="sr-only">
        {status === 'copied'
          ? t('common.action.copied')
          : status === 'failed'
            ? t('common.error_page.copy_failed')
            : null}
      </p>
    </div>
  )
}

export interface ErrorPageShellProps {
  status: ErrorStatus
  title: ReactNode
  body: ReactNode
  actions?: ReactNode
  requestId?: string
  /** 503 的恢复横幅等，位于内容之上。 */
  banner?: ReactNode
  /** 错误由 fetch 网络错误（ApiError.kind === 'offline'）触发。 */
  networkFailure?: boolean
  /**
   * 是否在离线时换用「网络连接已断开」文案。404 是确定性结果（路由不存在），
   * 与网络无关，因此 404 默认不切换。
   */
  offlineAware?: boolean
}

export function ErrorPageShell({
  status,
  title,
  body,
  actions,
  requestId,
  banner,
  networkFailure = false,
  offlineAware = true,
}: ErrorPageShellProps) {
  const t = useErrorPageTranslate()
  const offlineNow = useOfflineStatus()
  const offline = offlineAware && status !== 404 && (networkFailure || offlineNow)
  const headingRef = useRef<HTMLHeadingElement>(null)

  // 焦点落在标题（UI §8）：屏幕阅读器先读到「发生了什么」，Tab 再进入操作。
  useEffect(() => {
    headingRef.current?.focus()
  }, [])

  return (
    <main
      id="main"
      className="flex min-h-screen flex-col items-center justify-center bg-surface-page px-4 py-12 text-ink-1"
    >
      <div className="w-full max-w-[560px] text-center">
        {banner}
        <StatusIcon status={status} />
        {/*
          状态码是页面在 0.5 秒内自我标识的主体（UI §8 图形策略），
          移动端降为 36/44（00 §5 三档），保留 tabular-nums 防止位数跳动。
        */}
        <p className="mt-4 font-mono text-[48px] font-semibold leading-[56px] tabular-nums text-ink-2 max-[767px]:text-[36px] max-[767px]:leading-[44px]">
          {status}
        </p>
        <h1
          ref={headingRef}
          tabIndex={-1}
          className="mt-4 text-xl font-semibold leading-7 text-ink-1 outline-none"
        >
          {offline ? t('common.error_page.offline.title') : title}
        </h1>
        <div className="mx-auto mt-2 max-w-[80ch] text-sm leading-[22px] text-ink-2">
          {offline ? t('common.error_page.offline.body') : body}
        </div>
        {actions ? (
          <div className="mt-8 flex flex-col items-stretch justify-center gap-3 sm:flex-row sm:items-center">
            {actions}
          </div>
        ) : null}
        {requestId ? <RequestIdRow requestId={requestId} t={t} /> : null}
      </div>
    </main>
  )
}
