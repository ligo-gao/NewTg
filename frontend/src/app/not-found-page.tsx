import { isSafePublicHref } from '@/features/public/nav-visibility'
import { usePublicStatus } from '@/lib/public-status'

import { ErrorActionLink, ErrorPageShell, useErrorPageTranslate } from './error-page-shell'
import { readOptionalSiteConfig } from './error-status-page'

export interface NotFoundPageProps {
  requestId?: string
  /** 测试/静态注入；默认从 `GET /api/status` 的结构化可选字段读取。 */
  docsUrl?: string
  pricingEnabled?: boolean
}

/**
 * F-09-11 404。`/404` 路由与 router 的 `notFoundComponent` 渲染同一个组件，
 * 两个入口不会漂移（web-00 §8.3）。
 *
 * 次按钮优先级（UI §11）：`site.docs_url` 非空 → 查看文档（新标签）；否则
 * `nav.pricing.enabled` → 查看定价；两者都不可用 → 不渲染次按钮。后端当前
 * 尚未下发这两个字段时必须隐藏，不能凭空指向不存在的页面。
 *
 * 路由入口刻意声明为**无参数**函数：TanStack Router 的 `notFoundComponent`
 * 以空 props 调用它，而「全部字段可选」的 props 类型在 TS weak-type 规则下
 * 与 `{}` 无共同属性、不可赋值。需要注入 props 的场景用
 * `NotFoundPageContent`。
 */
export function NotFoundPage() {
  return <NotFoundPageContent />
}

export function NotFoundPageContent({
  requestId,
  docsUrl,
  pricingEnabled,
}: NotFoundPageProps) {
  const t = useErrorPageTranslate()
  const status = usePublicStatus()
  const config = readOptionalSiteConfig(status.data)
  const rawDocs = docsUrl ?? config.docsUrl
  // Reuse the public URL allowlist (F-09-38): a stale `javascript:` row must
  // not become the "Read the docs" link target.
  const docs = isSafePublicHref(rawDocs) ? rawDocs?.trim() : undefined
  const pricing = pricingEnabled ?? config.pricingEnabled

  return (
    <ErrorPageShell
      status={404}
      title={t('common.error_page.404.title')}
      body={t('common.error_page.404.body')}
      requestId={requestId}
      actions={
        <>
          <ErrorActionLink href="/">{t('common.error_page.home')}</ErrorActionLink>
          {docs !== undefined ? (
            <ErrorActionLink href={docs} variant="secondary" external>
              {t('common.error_page.docs')}
            </ErrorActionLink>
          ) : pricing ? (
            <ErrorActionLink href="/pricing" variant="secondary">
              {t('common.error_page.pricing')}
            </ErrorActionLink>
          ) : null}
        </>
      }
    />
  )
}
