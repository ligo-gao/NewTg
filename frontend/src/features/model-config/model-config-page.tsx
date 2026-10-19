import { useQuery } from '@tanstack/react-query'

import { publicStatusQueryOptions } from '@/lib/public-status'
import { selfSettingsQueryOptions } from '@/lib/self-settings'
import { useI18n } from '@/shared/lib/i18n-context'

import { DefaultModelCard } from './default-model-card'
import { KeyLimitsCard } from './key-limits-card'

export interface ModelConfigPageProps {
  /** `/model-config?focus=default-model` 的落地参数。 */
  focusDefaultModel?: boolean
}

/**
 * 模型配置页（modules-02 §2.15，F-02-18）。
 *
 * 两卡共用一个数据源：`GET /api/status.usable_groups` 是「用户能用哪些模型」
 * 的唯一真源；默认模型与令牌模型限制都只从它取候选，绝不展示用户不可用
 * 分组的模型。`GET /api/self` 只负责默认模型的当前值与乐观锁令牌。
 */
export function ModelConfigPage({
  focusDefaultModel = false,
}: ModelConfigPageProps) {
  const { t } = useI18n()
  const statusQuery = useQuery(publicStatusQueryOptions())
  const settingsQuery = useQuery(selfSettingsQueryOptions())

  const groups = statusQuery.data?.usable_groups ?? []
  const groupsFailed = statusQuery.isError

  return (
    <div className="mx-auto flex max-w-5xl flex-col gap-6">
      <header>
        <h1 className="text-xl font-semibold text-ink-1">
          {t('account.model_config.title')}
        </h1>
        <p className="mt-1 text-[13px] text-ink-2">
          {t('account.model_config.description')}
        </p>
      </header>

      <DefaultModelCard
        groups={groups}
        settings={settingsQuery.data}
        loading={statusQuery.isPending || settingsQuery.isPending}
        error={groupsFailed || settingsQuery.isError}
        onRetry={() => {
          void statusQuery.refetch()
          void settingsQuery.refetch()
        }}
        focus={focusDefaultModel}
      />

      <KeyLimitsCard groups={groups} groupsError={groupsFailed} />
    </div>
  )
}
