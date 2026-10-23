import type { JSX, ReactNode } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

export interface PageHeaderCrumb {
  label: string
  /** Present on ancestor crumbs; the current page is text only. */
  to?: string
}

export interface PageHeaderProps {
  crumbs: readonly PageHeaderCrumb[]
  title: string
  description?: string
  actions?: ReactNode
}

/**
 * Shared console page header (spec §6). The page owns the outer margin; this
 * component only lays out the breadcrumb, title, description and actions so
 * every console page keeps the same rhythm.
 */
export function PageHeader({
  crumbs,
  title,
  description,
  actions,
}: PageHeaderProps): JSX.Element {
  const { t } = useI18n()

  return (
    <div
      data-testid="page-header"
      className="flex flex-wrap items-start justify-between gap-3"
    >
      <div className="min-w-0">
        <nav aria-label={t('common.console.label')}>
          <ol className="mb-[7px] flex flex-wrap items-center gap-[6px] text-[11.5px]">
            {crumbs.map((crumb, index) => {
              // Only the final crumb is the current page (spec §6). Ancestors
              // stay plain text even when they carry no link — marking every
              // `to`-less crumb `aria-current` announced "current page" three
              // or four times per page.
              const current = index === crumbs.length - 1
              return (
                <li
                  key={`${crumb.label}-${index}`}
                  className="flex items-center gap-[6px]"
                >
                  {index === 0 ? null : (
                    <span aria-hidden="true" className="text-ink-3">
                      /
                    </span>
                  )}
                  {crumb.to === undefined ? (
                    <span
                      aria-current={current ? 'page' : undefined}
                      className="text-ink-3"
                    >
                      {crumb.label}
                    </span>
                  ) : (
                    <a href={crumb.to} className="text-ink-3 hover:text-ink-1">
                      {crumb.label}
                    </a>
                  )}
                </li>
              )
            })}
          </ol>
        </nav>
        <h1 className="text-[21px] leading-7 font-semibold tracking-[-0.01em] text-ink-1">
          {title}
        </h1>
        {description === undefined ? null : (
          <p className="mt-[3px] max-w-[80ch] text-[13px] leading-5 text-ink-2">
            {description}
          </p>
        )}
      </div>
      {actions === undefined ? null : (
        <div className="flex flex-wrap items-center gap-2">{actions}</div>
      )}
    </div>
  )
}
