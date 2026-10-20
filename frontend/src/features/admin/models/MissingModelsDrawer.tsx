import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'

export interface MissingModelsDrawerProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** 「补建档」：关闭抽屉并打开新建抽屉（预填模型名由父级负责）。 */
  onCreateRecord: (modelName: string) => void
  canManage: boolean
}

/**
 * F-06-26 缺失模型（只读）。
 *
 * 数据源是启用渠道 `supported_models` 的并集减去已有档案，**不是**
 * `abilities` 表（NEW TG 已裁掉该派生表）。抽屉本身没有任何写面：
 * 「补建档」只是打开 F-06-22 的新建抽屉，真正写入仍走那条路径的权限。
 */
export function MissingModelsDrawer({
  open,
  onOpenChange,
  onCreateRecord,
  canManage,
}: MissingModelsDrawerProps) {
  const { t } = useI18n()

  const query = useQuery({
    ...missingModelsQueryOptions(),
    enabled: open,
  })

  const data = query.data
  const items = data?.items ?? []

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      variant="sheet"
      panelClassName="!max-w-[520px] xl:!max-w-[560px]"
      title={t('admin.models.missing_title')}
      description={t('admin.models.missing_hint')}
    >
      {query.isPending ? (
        <div className="space-y-2" aria-busy="true">
          {[0, 1, 2, 3].map((index) => (
            <div key={index} className="h-9 animate-pulse rounded bg-surface-subtle" />
          ))}
        </div>
      ) : query.isError ? (
        <div className="flex items-center gap-3 text-sm text-ink-2">
          <span>{t('admin.models.missing_error')}</span>
          <Button variant="secondary" onClick={() => void query.refetch()}>
            {t('common.action.retry')}
          </Button>
        </div>
      ) : items.length === 0 ? (
        <p className="text-sm text-ink-2">{t('admin.models.missing_empty')}</p>
      ) : (
        <>
          {data !== undefined && data.skipped_channels > 0 ? (
            <p className="mb-2 text-xs text-warning">
              {t('admin.models.missing_skipped', { count: data.skipped_channels })}
            </p>
          ) : null}
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
                <th scope="col" className="py-2 pr-3 font-medium">
                  {t('admin.models.missing_column_model')}
                </th>
                <th scope="col" className="py-2 pr-3 text-right font-medium">
                  {t('admin.models.missing_column_channels')}
                </th>
                <th scope="col" className="py-2 pr-3 font-medium">
                  {t('admin.models.missing_column_pricing')}
                </th>
                <th scope="col" className="py-2 text-right font-medium">
                  {t('admin.models.actions')}
                </th>
              </tr>
            </thead>
            <tbody>
              {items.map((item) => (
                <tr key={item.model_name} className="border-b border-border-hairline last:border-0">
                  <td className="py-2 pr-3 font-mono text-ink-1">{item.model_name}</td>
                  <td className="py-2 pr-3 text-right font-mono tabular-nums text-ink-1">
                    {item.channel_count}
                  </td>
                  <td className="py-2 pr-3 text-xs text-ink-2">
                    {item.has_pricing
                      ? t('admin.models.missing_has_pricing')
                      : t('admin.models.missing_no_pricing')}
                  </td>
                  <td className="py-2 text-right">
                    {canManage ? (
                      <Button
                        variant="ghost"
                        className="h-8 px-2 text-[13px]"
                        onClick={() => {
                          onCreateRecord(item.model_name)
                        }}
                      >
                        {t('admin.models.missing_create')}
                      </Button>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </Dialog>
  )
}

/** 供父级徽章复用同一查询键与请求，避免打开抽屉时再发一次。 */
export function missingModelsQueryOptions() {
  return {
    queryKey: ['admin-models-missing'] as const,
    queryFn: async () => {
      return requireData(await apiClient.GET('/api/models/missing'))
    },
  }
}
