import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert } from '@/shared/ui/form'

import {
  installPluginFromMarketplace,
  type MarketplacePlugin,
  type MarketplaceVersion,
} from './api'
import { notifySuccess } from './notify'
import { errorMessage, problemSlug, shortHash } from './utils'

/** 市场安装错误的用户面文案（§6.10 最终文案表）。 */
const ERROR_KEYS: Record<string, string> = {
  plugin_hash_mismatch: 'admin.task_plugins.marketplace.hash_mismatch',
  plugin_package_mismatch: 'admin.task_plugins.marketplace.package_mismatch',
  plugin_version_conflict: 'admin.task_plugins.marketplace.version_conflict',
  plugin_marketplace_disabled: 'admin.task_plugins.marketplace.disabled',
}

export interface InstallDialogProps {
  plugin: MarketplacePlugin | null
  canManage: boolean
  online: boolean
  onClose: () => void
}

/**
 * 市场安装对话框（F-08-33）。
 *
 * 「立即激活」默认不勾：新版本必须能继续解析在途任务的响应，激活是显式的
 * 第二个决定（S4）。hash 不符时服务端零写入，界面就地给出「来源可能被篡改，
 * 已拒绝安装」——错误文本可复制，不用一闪而过的 Toast。
 */
export function InstallDialog({
  plugin,
  canManage,
  online,
  onClose,
}: InstallDialogProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [version, setVersion] = useState('')
  const [activate, setActivate] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<{ slug: string; message: string } | null>(null)

  useEffect(() => {
    if (plugin === null) return
    setVersion(plugin.latest_version)
    // 默认不激活是冻结语义（R-M6D-07）：每次打开都重置。
    setActivate(false)
    setError(null)
    setSubmitting(false)
  }, [plugin])

  if (plugin === null) return null
  // 提前取非空引用：下面的闭包（install）在 TS 眼里不再享有 null 收窄。
  const target: MarketplacePlugin = plugin

  const available: MarketplaceVersion[] = [...target.versions].sort((a, b) =>
    a.version === b.version ? 0 : a.version < b.version ? 1 : -1,
  )
  const selected = available.find((entry) => entry.version === version) ?? available[0]

  async function install(): Promise<void> {
    if (selected === undefined) return
    setSubmitting(true)
    setError(null)
    try {
      await installPluginFromMarketplace(
        {
          source_id: selected.source_id,
          key: target.key,
          version: selected.version,
          activate,
        },
      )
      notifySuccess(
        t(
          activate
            ? 'admin.task_plugins.marketplace.install_activated'
            : 'admin.task_plugins.marketplace.install_success',
          { version: selected.version },
        ),
      )
      void queryClient.invalidateQueries({ queryKey: ['plugin-list'] })
      void queryClient.invalidateQueries({ queryKey: ['plugin-marketplace'] })
      void queryClient.invalidateQueries({ queryKey: ['plugin-runtime'] })
      onClose()
    } catch (installError) {
      setError({
        slug: problemSlug(installError),
        message: errorMessage(installError),
      })
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      open
      onOpenChange={(next) => {
        if (!next) onClose()
      }}
      title={t('admin.task_plugins.marketplace.install_title', { name: target.name })}
      description={target.description}
      dismissible={!submitting}
      footer={
        <>
          <Button variant="secondary" disabled={submitting} onClick={onClose}>
            {t('admin.task_plugins.common.cancel')}
          </Button>
          <Button
            disabled={!canManage || !online || submitting || selected === undefined}
            onClick={() => void install()}
          >
            {submitting
              ? t('admin.task_plugins.marketplace.installing')
              : t('admin.task_plugins.marketplace.install')}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <label className="flex flex-col gap-1 text-meta text-ink-3">
          {t('admin.task_plugins.marketplace.version_label')}
          <select
            value={selected?.version ?? ''}
            onChange={(event) => setVersion(event.target.value)}
            className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1"
          >
            {available.map((entry) => (
              <option key={`${entry.source_id}-${entry.version}`} value={entry.version}>
                {entry.version}
                {entry.installed
                  ? ` · ${t('admin.task_plugins.marketplace.installed')}`
                  : ''}
              </option>
            ))}
          </select>
        </label>

        {selected !== undefined ? (
          <dl className="grid grid-cols-1 gap-2 rounded-lg bg-surface-subtle p-3 text-body sm:grid-cols-2">
            <div>
              <dt className="text-meta text-ink-3">
                {t('admin.task_plugins.marketplace.summary_version')}
              </dt>
              <dd className="font-mono">{selected.version}</dd>
            </div>
            <div>
              <dt className="text-meta text-ink-3">
                {t('admin.task_plugins.marketplace.summary_hash')}
              </dt>
              <dd className="font-mono" title={selected.source_hash}>
                {shortHash(selected.source_hash)}
              </dd>
            </div>
            <div className="sm:col-span-2">
              <dt className="text-meta text-ink-3">
                {t('admin.task_plugins.marketplace.summary_source')}
              </dt>
              <dd className="break-all font-mono text-meta">{selected.source_url}</dd>
            </div>
          </dl>
        ) : null}

        <div>
          <label className="flex items-center gap-2 text-body text-ink-1">
            <input
              type="checkbox"
              checked={activate}
              onChange={(event) => setActivate(event.target.checked)}
            />
            {t('admin.task_plugins.marketplace.activate_now')}
          </label>
          <p className="mt-1 text-meta text-ink-3">
            {t('admin.task_plugins.marketplace.activate_hint')}
          </p>
        </div>

        {error !== null ? (
          <FormAlert>
            {ERROR_KEYS[error.slug] !== undefined
              ? t(ERROR_KEYS[error.slug] ?? '')
              : error.message}
          </FormAlert>
        ) : null}

        {!online ? (
          <p className="text-meta text-ink-3">{t('admin.task_plugins.common.offline')}</p>
        ) : null}
      </div>
    </Dialog>
  )
}
