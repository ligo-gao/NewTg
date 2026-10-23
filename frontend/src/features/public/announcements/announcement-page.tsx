/**
 * 公开公告详情页（F-08-04 用户侧公开只读视图）。
 *
 * 数据来自 `GET /api/announcements/public/{id}`：只有**已发布且生效中**
 * 的公告会返回 200，其余一律 404 problem+json（不透露存在性）。页面因此
 * 把 404 渲染成独立空态，而不是通用错误。
 *
 * 六态：加载骨架 / 404 空态 / 429 倒计时 / 离线 / 其它错误重试 / 正常。
 * `expire_at` 到期只切换一个本地标记，**不自动发请求**（服务端下一次读取
 * 会直接 404；已打开的页面保留内容并提示「已结束」）。
 */

import { useQuery } from '@tanstack/react-query'
import { useEffect, useState } from 'react'

import { ApiError } from '@/api/errors'
import { htmlToPlainText } from '@/features/public/home/custom-html'
import { useI18n } from '@/shared/lib/i18n-context'
import { useCountdown } from '@/shared/lib/use-countdown'
import { Tag, type TagVariant } from '@/shared/ui/tag'

import { publicAnnouncementQueryOptions } from './api'
import { useAnnouncementSeo } from './seo'

/**
 * `Retry-After` 缺失时的兜底。契约冻结 B8 = 60 次/分钟，因此默认窗口取
 * 60 秒；服务端给出头时永远以头为准（见 `ApiError.retry_after`）。
 */
const RATE_LIMIT_FALLBACK_SECONDS = 60

const CATEGORY_VARIANTS: Record<string, TagVariant> = {
  notice: 'info',
  maintenance: 'warning',
  release: 'success',
}

function formatDateTime(iso: string, locale: string): string {
  const timestamp = Date.parse(iso)
  if (Number.isNaN(timestamp)) return '—'
  return new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(timestamp)
}

function isExpiredAt(iso: string | null | undefined, now: number): boolean {
  if (iso === null || iso === undefined || iso === '') return false
  const timestamp = Date.parse(iso)
  return !Number.isNaN(timestamp) && timestamp <= now
}

/**
 * 到期标记。定时器只改本地状态，不触发查询；`setTimeout` 的上限约 24.8 天，
 * 更远的到期时间会在上限处重新检查一次（公告生命周期远短于该值）。
 */
function useExpired(expireAt: string | null | undefined): boolean {
  const [expired, setExpired] = useState(() => isExpiredAt(expireAt, Date.now()))

  useEffect(() => {
    if (expireAt === null || expireAt === undefined || expireAt === '') {
      setExpired(false)
      return
    }
    const timestamp = Date.parse(expireAt)
    if (Number.isNaN(timestamp)) {
      setExpired(false)
      return
    }
    const update = () => setExpired(timestamp <= Date.now())
    update()
    if (timestamp <= Date.now()) return

    const timer = setTimeout(
      update,
      Math.min(timestamp - Date.now(), 2_147_483_647),
    )
    return () => clearTimeout(timer)
  }, [expireAt])

  return expired
}

export interface AnnouncementPageProps {
  id: string
}

function StateIcon({ kind }: { kind: 'not_found' | 'error' }) {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 24 24"
      className="size-6 text-ink-4"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
    >
      {kind === 'not_found' ? (
        <>
          <path d="M6 3h8l4 4v14H6z" />
          <path d="M14 3v4h4M9 12h6M9 16h4" strokeLinecap="round" />
        </>
      ) : (
        <>
          <circle cx="12" cy="12" r="9" />
          <path d="M12 7v6M12 16.5v.5" strokeLinecap="round" />
        </>
      )}
    </svg>
  )
}

export function AnnouncementPage({ id }: AnnouncementPageProps) {
  const { t, locale } = useI18n()
  const query = useQuery(publicAnnouncementQueryOptions(id))
  const data = query.data

  const title =
    data === undefined
      ? null
      : data.title.trim() === ''
        ? t('public.announcements.untitled')
        : data.title.trim()
  const description =
    data === undefined
      ? null
      : htmlToPlainText(data.content_html) || title
  useAnnouncementSeo(title, description)

  const expired = useExpired(data?.expire_at)
  const { seconds, running, start } = useCountdown()

  const error = query.error
  const notFound = error instanceof ApiError && error.kind === 'not_found'
  const rateLimited =
    error instanceof ApiError && error.kind === 'rate_limited'
  const offline = error instanceof ApiError && error.kind === 'offline'

  const retryAfter =
    rateLimited && error instanceof ApiError
      ? (error.retry_after ?? RATE_LIMIT_FALLBACK_SECONDS)
      : null

  // 服务端倒计时：只在错误对象变化时启动一次；倒计时结束不会自动重查，
  // 由用户显式点「重试」。
  useEffect(() => {
    if (retryAfter === null) return
    start(retryAfter)
  }, [retryAfter, start])

  if (query.isPending) {
    return (
      <div aria-busy="true" className="flex w-full flex-col gap-4">
        <span className="sr-only">{t('public.announcements.loading')}</span>
        <div className="h-5 w-24 rounded bg-surface-subtle" />
        <div className="h-7 w-2/3 rounded bg-surface-subtle" />
        <div className="h-3 w-full rounded bg-surface-subtle" />
        <div className="h-3 w-5/6 rounded bg-surface-subtle" />
        <div className="h-3 w-4/6 rounded bg-surface-subtle" />
      </div>
    )
  }

  if (query.isError) {
    if (notFound) {
      return (
        <div
          role="status"
          className="mx-auto flex min-h-[320px] w-full max-w-[760px] flex-col items-center justify-center gap-3 rounded-xl border border-border-hairline bg-surface-card px-6 text-center"
        >
          <StateIcon kind="not_found" />
          <h1 className="text-base font-semibold text-ink-1">
            {t('public.announcements.not_found_title')}
          </h1>
          <p className="max-w-[480px] text-[13px] leading-5 text-ink-2">
            {t('public.announcements.not_found_hint')}
          </p>
          <div className="mt-1 flex flex-wrap items-center justify-center gap-2">
            <a
              href="/"
              className="inline-flex h-9 items-center rounded-lg bg-brand px-4 text-sm font-medium text-on-brand hover:bg-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              {t('public.announcements.back_home')}
            </a>
            <a
              href="/pricing"
              className="inline-flex h-9 items-center rounded-lg border border-border-default px-4 text-sm font-medium text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              {t('public.announcements.back_pricing')}
            </a>
          </div>
        </div>
      )
    }

    const message = rateLimited
      ? running
        ? t('public.announcements.rate_limited_countdown', { seconds })
        : t('public.announcements.rate_limited')
      : offline
        ? t('public.announcements.offline')
        : t('public.announcements.error')

    return (
      <div
        role="alert"
        className="flex w-full flex-col items-start gap-3 rounded-xl border border-danger-line bg-danger-bg px-5 py-4 text-danger"
      >
        <StateIcon kind="error" />
        <h1 className="text-base font-semibold text-ink-1">
          {t('public.announcements.error_title')}
        </h1>
        <p className="text-sm leading-[22px] text-ink-1">{message}</p>
        <button
          type="button"
          disabled={rateLimited && running}
          onClick={() => void query.refetch()}
          className="inline-flex h-9 items-center rounded-lg border border-border-default bg-surface-card px-4 text-sm font-medium text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-60"
        >
          {t('public.announcements.retry')}
        </button>
      </div>
    )
  }

  const announcement = query.data
  const hasContent = announcement.content_html.trim() !== ''

  return (
    <article
      aria-labelledby="announcement-title"
      className="flex w-full flex-col gap-6"
    >
      <header className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-[13px] leading-5 text-ink-2">
          <Tag
            variant={CATEGORY_VARIANTS[announcement.category] ?? 'neutral'}
            ariaLabel={t('public.announcements.category_label')}
          >
            {t(`public.announcements.category.${announcement.category}`)}
          </Tag>
          <span className="inline-flex items-center gap-1">
            <span>{t('public.announcements.published_at')}</span>
            <time dateTime={announcement.published_at}>
              {formatDateTime(announcement.published_at, locale)}
            </time>
          </span>
          {announcement.expire_at !== null ? (
            <span className="inline-flex items-center gap-1">
              <span>{t('public.announcements.expire_at')}</span>
              <time dateTime={announcement.expire_at}>
                {formatDateTime(announcement.expire_at, locale)}
              </time>
            </span>
          ) : null}
        </div>
        <h1
          id="announcement-title"
          className="text-xl font-semibold leading-7 text-ink-1"
        >
          {announcement.title.trim() === ''
            ? t('public.announcements.untitled')
            : announcement.title.trim()}
        </h1>
      </header>

      {expired ? (
        <div
          role="status"
          className="flex flex-col gap-1 rounded-lg border border-warning-line bg-warning-bg px-4 py-3 text-sm text-warning"
        >
          <span className="font-medium">{t('public.announcements.ended')}</span>
          <span className="text-[13px] leading-5 text-ink-2">
            {t('public.announcements.ended_hint')}
          </span>
        </div>
      ) : null}

      {hasContent ? (
        // `content_html` 由服务端按 `SanitizeMarkdown`（security-04 §2.5）
        // 生成并净化：原始 HTML 不透传、事件属性被剥离、链接协议受白名单
        // 限制。这是全页**唯一**允许注入 HTML 的字段；不要用它渲染
        // 客户端拼接的字符串。
        <div
          className="flex flex-col text-sm leading-[22px] text-ink-1 [&_a]:text-brand [&_a]:underline [&_blockquote]:border-l-2 [&_blockquote]:border-border-default [&_blockquote]:pl-3 [&_blockquote]:text-ink-2 [&_code]:rounded [&_code]:bg-surface-subtle [&_code]:px-1 [&_h1]:mt-6 [&_h1]:text-xl [&_h1]:font-semibold [&_h2]:mt-6 [&_h2]:text-lg [&_h2]:font-semibold [&_h3]:mt-4 [&_h3]:text-base [&_h3]:font-semibold [&_h4]:mt-4 [&_h4]:font-semibold [&_hr]:my-4 [&_hr]:border-border-hairline [&_li]:mt-1 [&_ol]:list-decimal [&_ol]:pl-5 [&_p]:mt-3 [&_pre]:mt-3 [&_pre]:overflow-x-auto [&_pre]:rounded-lg [&_pre]:bg-surface-subtle [&_pre]:p-3 [&_strong]:font-semibold [&_ul]:list-disc [&_ul]:pl-5"
          dangerouslySetInnerHTML={{ __html: announcement.content_html }}
        />
      ) : (
        <p className="text-sm leading-[22px] text-ink-2">
          {t('public.announcements.empty_content')}
        </p>
      )}
    </article>
  )
}
