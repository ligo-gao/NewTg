import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'

import { meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'

import { ModelsTab } from './ModelsTab'
import {
  canReadCatalog,
  type ModelCatalogSearch,
  type ModelCatalogTab,
} from './model-catalog-search'
import { VendorsTab } from './VendorsTab'

const TABS: readonly ModelCatalogTab[] = ['models', 'vendors']

/** `<1024` 管理页闸门（modules-06 UI §7）；无 matchMedia 的环境按桌面处理。 */
function useMinWidth(px: number): boolean {
  const [matches, setMatches] = useState(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
      return true
    }
    return window.matchMedia(`(min-width: ${px}px)`).matches
  })
  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
      return
    }
    const query = window.matchMedia(`(min-width: ${px}px)`)
    const update = () => setMatches(query.matches)
    update()
    query.addEventListener('change', update)
    return () => query.removeEventListener('change', update)
  }, [px])
  return matches
}

export interface ModelCatalogPageProps {
  search: ModelCatalogSearch
  onSearchChange: (next: ModelCatalogSearch, options?: { replace?: boolean }) => void
}

/**
 * F-06-22～26 模型与供应商目录（`/admin/models`，双 Tab）。
 *
 * Tab 切换只改 URL 的 `tab` 键；两侧筛选各自带 `mv_` / `vv_` 前缀，
 * 因此来回切换不会丢失另一侧的工作状态，也不会让一次切换触发两次带错
 * 参数的列表请求。
 *
 * 权限分层（任务书 §1）：读 `channels:read`（路由 beforeLoad 已拦）、
 * 写 `channels:manage`、批量/同步/供应商操作 `channels:manage_sensitive`；
 * `clear_pricing` 额外要求 `pricing:manage`。按钮在无权限时禁用或隐藏，
 * 服务端仍逐请求强制。
 */
export function ModelCatalogPage({ search, onSearchChange }: ModelCatalogPageProps) {
  const { t } = useI18n()
  const wideEnough = useMinWidth(1024)
  const meQuery = useQuery(meQueryOptions())
  const permissions = meQuery.data?.permissions

  // 路由 beforeLoad 已保证 channels:read；能力未知时先渲染骨架，避免把
  // 注定 403 的请求发出去。
  if (meQuery.isPending) {
    return (
      <div className="mx-auto flex max-w-[1520px] flex-col gap-3">
        <h1 className="text-xl font-semibold text-ink-1">{t('admin.models.title')}</h1>
        <section className="rounded-xl border border-border-hairline bg-surface-card p-5" aria-busy="true">
          {[0, 1, 2, 3, 4].map((index) => (
            <div key={index} className="mb-2 h-10 animate-pulse rounded bg-surface-subtle" />
          ))}
        </section>
      </div>
    )
  }

  if (!canReadCatalog(permissions)) {
    return (
      <div className="mx-auto max-w-[1520px] rounded-xl border border-border-hairline bg-surface-card p-8">
        <h1 className="text-xl font-semibold text-ink-1">{t('common.error403.title')}</h1>
        <p className="mt-2 text-sm text-ink-2">{t('common.error403.body')}</p>
      </div>
    )
  }

  if (!wideEnough) {
    return (
      <div className="mx-auto max-w-[1520px] rounded-xl border border-border-hairline bg-surface-card p-8">
        <h1 className="text-xl font-semibold text-ink-1">{t('admin.models.title')}</h1>
        <p className="mt-2 text-sm text-ink-2">{t('admin.models.desktop_only')}</p>
      </div>
    )
  }

  return (
    <div className="mx-auto flex max-w-[1520px] flex-col gap-3">
      <div className="flex flex-wrap items-end justify-between gap-2">
        <div>
          <h1 className="text-xl font-semibold text-ink-1">{t('admin.models.title')}</h1>
          <p className="mt-1 text-sm text-ink-2">{t('admin.models.subtitle')}</p>
        </div>
        <div role="tablist" aria-label={t('admin.models.title')} className="flex gap-1 border-b border-border-hairline">
          {TABS.map((tab) => {
            const active = search.tab === tab
            return (
              <button
                key={tab}
                type="button"
                role="tab"
                aria-selected={active}
                className={[
                  'border-b-2 px-3 py-2 text-sm',
                  active
                    ? 'border-brand font-medium text-ink-1'
                    : 'border-transparent text-ink-2 hover:text-ink-1',
                ].join(' ')}
                onClick={() => {
                  if (active) return
                  // 只改 tab：两侧筛选（mv_*/vv_*）原样保留。
                  onSearchChange({ ...search, tab }, { replace: true })
                }}
              >
                {tab === 'models'
                  ? t('admin.models.tab_models')
                  : t('admin.models.tab_vendors')}
              </button>
            )
          })}
        </div>
      </div>

      {search.tab === 'models' ? (
        <ModelsTab search={search} onSearchChange={onSearchChange} permissions={permissions} />
      ) : (
        <VendorsTab search={search} onSearchChange={onSearchChange} permissions={permissions} />
      )}
    </div>
  )
}
