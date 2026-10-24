import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'

import { listTemplates } from './api'
import { NOTIFICATION_CATEGORIES } from './registry'

export interface NotifyEntryCardProps {
  /** 缺少 settings:manage 时不渲染入口（服务端仍会拒绝写请求）。 */
  canManage: boolean
  /** 测试/嵌入场景可用回调替代路由跳转。 */
  onManage?: () => void
}

function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : String(error)
}

/**
 * 设置壳通知渠道内的「通知模板」入口卡（UI §6.9 信息架构）。
 *
 * 只展示覆盖概况：16 类来自前端冻结注册表，已修改行数与语言覆盖来自
 * 列表端点返回的真实行——不在这里复制后端统计口径。
 */
export function NotifyEntryCard({ canManage, onManage }: NotifyEntryCardProps) {
  const { t } = useI18n()

  const query = useQuery({
    queryKey: ['notification-templates', 'coverage'],
    queryFn: ({ signal }) => listTemplates({}, signal),
    enabled: canManage,
  })

  if (!canManage) return null

  const items = query.data?.items ?? []
  const modified = items.filter((item) => item.customized).length
  const locales = new Set(items.map((item) => item.locale)).size

  return (
    <section
      data-testid="settings-notify-entry"
      aria-labelledby="settings-notify-entry-title"
      className="rounded-xl border border-border-default bg-surface-card p-5"
    >
      <header className="border-b border-border-hairline pb-3">
        <h2
          id="settings-notify-entry-title"
          className="text-card-title font-semibold leading-[22px] text-ink-1"
        >
          {t('settings.notification_templates.entry.title')}
        </h2>
        <p className="mt-0.5 text-meta text-ink-3">
          {t('settings.notification_templates.entry.description')}
        </p>
      </header>

      {query.isError ? (
        <div className="space-y-3 pt-4">
          <FormAlert tone="danger">
            {t('settings.notification_templates.entry.load_error')}{' '}
            {errorMessage(query.error)}
          </FormAlert>
          <Button variant="secondary" onClick={() => void query.refetch()}>
            {t('settings.notification_templates.entry.retry')}
          </Button>
        </div>
      ) : null}

      {query.isPending ? (
        <div className="space-y-2 pt-4" aria-hidden="true">
          <div className="h-4 w-64 rounded bg-surface-subtle" />
          <div className="h-9 w-36 rounded-lg bg-surface-subtle" />
        </div>
      ) : null}

      {query.isSuccess ? (
        <div className="flex flex-wrap items-center justify-between gap-3 pt-4">
          <dl className="flex flex-wrap gap-x-6 gap-y-1 text-body text-ink-2">
            <div>
              <dt className="text-meta text-ink-3">
                {t('settings.notification_templates.entry.categories_label')}
              </dt>
              <dd className="font-medium text-ink-1">{NOTIFICATION_CATEGORIES.length}</dd>
            </div>
            <div>
              <dt className="text-meta text-ink-3">
                {t('settings.notification_templates.entry.modified_label')}
              </dt>
              <dd className="font-medium text-ink-1">{modified}</dd>
            </div>
            <div>
              <dt className="text-meta text-ink-3">
                {t('settings.notification_templates.entry.locales_label')}
              </dt>
              <dd className="font-medium text-ink-1">{locales}</dd>
            </div>
          </dl>
          {onManage !== undefined ? (
            <Button onClick={onManage}>
              {t('settings.notification_templates.entry.manage')}
            </Button>
          ) : (
            <Link to="/admin/settings/notification-templates">
              <Button>{t('settings.notification_templates.entry.manage')}</Button>
            </Link>
          )}
        </div>
      ) : null}
    </section>
  )
}
