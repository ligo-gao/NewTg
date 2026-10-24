import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'

import {
  fetchDeploymentSettings,
  saveDeploymentSettings,
  testDeploymentConnection,
  type DeployConnectionTestResult,
} from './api'
import { ConfirmDialog, SectionCard } from './components'
import { SecretField } from './secret-field'
import { useOnlineStatus } from './use-online'
import {
  errorMessage,
  isForbidden,
  isRateLimited,
  retryAfterOf,
} from './utils'

/**
 * 设置 Tab（F-08-51）。只有两个键值走 section 语义；写操作在页面层做
 * DangerConfirm（关闭开关 / 清除密钥），提交复用同一 `updated_at` 乐观锁。
 * 连接测试是只读探测：候选密钥只在本次请求中使用，绝不落库。
 */
export function SettingsTab({ canManage }: { canManage: boolean }) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const online = useOnlineStatus()

  const query = useQuery({
    queryKey: ['deployment-settings'],
    queryFn: ({ signal }) => fetchDeploymentSettings(signal),
    retry: false,
  })
  const settings = query.data

  const [enabled, setEnabled] = useState(false)
  const [apiKey, setApiKey] = useState<string | null>('')
  const [savedNotice, setSavedNotice] = useState(false)
  const [confirm, setConfirm] = useState<'disable' | 'clear' | null>(null)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<unknown>(null)

  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<DeployConnectionTestResult | null>(null)
  const [testError, setTestError] = useState<unknown>(null)
  const [countdown, setCountdown] = useState(0)

  // 服务端数据到达/刷新后重置草稿。
  useEffect(() => {
    if (settings === undefined) return
    setEnabled(settings.enabled)
    setApiKey('')
  }, [settings])

  useEffect(() => {
    if (countdown <= 0) return
    const timer = globalThis.setTimeout(() => setCountdown((value) => Math.max(0, value - 1)), 1000)
    return () => globalThis.clearTimeout(timer)
  }, [countdown])

  const dirty =
    settings !== undefined &&
    (enabled !== settings.enabled || apiKey !== '')

  const requestClear = (): void => setConfirm('clear')

  const reset = (): void => {
    if (settings === undefined) return
    setEnabled(settings.enabled)
    setApiKey('')
    setSaveError(null)
    setSavedNotice(false)
  }

  const save = async (): Promise<void> => {
    if (settings === undefined || saving) return
    setSaving(true)
    setSaveError(null)
    setSavedNotice(false)
    try {
      const body: Parameters<typeof saveDeploymentSettings>[0] = {
        enabled,
        updated_at: settings.updated_at,
      }
      if (apiKey === null) body.clear_api_key = true
      else if (apiKey !== '') body.api_key = apiKey
      const next = await saveDeploymentSettings(body)
      queryClient.setQueryData(['deployment-settings'], next)
      setApiKey('')
      setSavedNotice(true)
    } catch (cause) {
      setSaveError(cause)
    } finally {
      setSaving(false)
    }
  }

  const requestSave = (): void => {
    if (settings === undefined) return
    // 关闭启用 = 部署能力立即失效；清除密钥同理，都必须二次确认。
    if (settings.enabled && !enabled) {
      setConfirm('disable')
      return
    }
    void save()
  }

  const runTest = async (): Promise<void> => {
    if (testing || countdown > 0) return
    setTesting(true)
    setTestError(null)
    setTestResult(null)
    try {
      const result = await testDeploymentConnection(
        apiKey === null || apiKey === '' ? undefined : apiKey,
      )
      setTestResult(result)
    } catch (cause) {
      if (isRateLimited(cause)) setCountdown(retryAfterOf(cause) ?? 60)
      setTestError(cause)
    } finally {
      setTesting(false)
    }
  }

  const writeDisabled = !canManage || !online || saving
  const retryAfter = retryAfterOf(saveError)

  return (
    <section
      className="space-y-4"
      aria-label={t('admin.deployments.page.tab_settings')}
      data-testid="deployments-settings-tab"
    >
      <SectionCard
        title={t('admin.deployments.settings.title')}
        description={t('admin.deployments.settings.subtitle')}
      >
        {query.isPending ? (
          <p className="text-body text-ink-2" role="status">
            {t('admin.deployments.common.loading')}
          </p>
        ) : null}
        {query.isError ? (
          <div className="space-y-2">
            <FormAlert tone="danger">
              {isForbidden(query.error)
                ? t('admin.deployments.common.forbidden')
                : `${t('admin.deployments.settings.load_error')} ${errorMessage(query.error)}`}
            </FormAlert>
            <Button variant="secondary" onClick={() => void query.refetch()}>
              {t('admin.deployments.common.retry')}
            </Button>
          </div>
        ) : null}

        {settings !== undefined ? (
          <div className="space-y-4">
            <div className="flex items-center justify-between gap-4 rounded-lg border border-border-default p-3">
              <div>
                <p className="text-meta font-medium text-ink-1">
                  {t('admin.deployments.settings.enabled')}
                </p>
                <p className="text-meta text-ink-3">
                  {t('admin.deployments.settings.enabled_hint')}
                </p>
              </div>
              <input
                type="checkbox"
                role="switch"
                aria-label={t('admin.deployments.settings.enabled')}
                checked={enabled}
                disabled={writeDisabled}
                onChange={(event) => {
                  setEnabled(event.target.checked)
                  setSavedNotice(false)
                }}
                className="size-5"
              />
            </div>

            <SecretField
              id="deploy-settings-api-key"
              label={t('admin.deployments.settings.api_key')}
              value={apiKey}
              configured={settings.has_secret}
              disabled={writeDisabled}
              hint={t('admin.deployments.settings.api_key_hint')}
              onRequestClear={requestClear}
              onChange={(value) => {
                setApiKey(value)
                setSavedNotice(false)
              }}
            />

            <div className="flex flex-wrap items-center gap-2">
              <Button
                variant="secondary"
                onClick={() => void runTest()}
                disabled={testing || countdown > 0 || !online}
              >
                {testing
                  ? t('admin.deployments.settings.testing')
                  : countdown > 0
                    ? t('admin.deployments.settings.rate_limited_countdown', {
                        seconds: countdown,
                      })
                    : t('admin.deployments.settings.test')}
              </Button>
              {testResult !== null ? (
                <p className="text-body text-success" role="status">
                  {t('admin.deployments.settings.test_ok', {
                    hardware: testResult.hardware_count,
                    available: testResult.total_available,
                    latency: testResult.latency_ms,
                  })}
                </p>
              ) : null}
              {testError !== null ? (
                <FormAlert tone="danger">
                  {isRateLimited(testError)
                    ? t('admin.deployments.settings.rate_limited')
                    : errorMessage(testError)}
                </FormAlert>
              ) : null}
            </div>

            <div className="rounded-lg border border-border-hairline bg-surface-subtle p-3 text-meta text-ink-2">
              <p>
                <span className="font-medium text-ink-1">
                  {t('admin.deployments.settings.egress')}:
                </span>{' '}
                api.io.solutions:443
              </p>
              <p className="mt-1">{t('admin.deployments.settings.notes')}</p>
            </div>

            {saveError !== null ? (
              <FormAlert
                tone={
                  saveError instanceof ApiError && saveError.status === 409
                    ? 'warning'
                    : 'danger'
                }
              >
                {errorMessage(saveError)}
                {retryAfter !== undefined
                  ? ` ${t('admin.deployments.settings.retry_after', { seconds: retryAfter })}`
                  : ''}
              </FormAlert>
            ) : null}
            {saveError instanceof ApiError && saveError.status === 409 ? (
              <Button
                variant="secondary"
                onClick={() => {
                  setSaveError(null)
                  void query.refetch()
                }}
              >
                {t('admin.deployments.settings.conflict_refresh')}
              </Button>
            ) : null}
            {savedNotice ? (
              <p className="text-body text-success" role="status">
                {t('admin.deployments.settings.saved')}
              </p>
            ) : null}
            {!online ? (
              <p className="text-meta text-ink-3">{t('admin.deployments.common.offline')}</p>
            ) : null}

            {canManage ? (
              <div className="flex justify-end gap-2 border-t border-border-hairline pt-3">
                <Button
                  variant="ghost"
                  onClick={reset}
                  disabled={!dirty || saving}
                >
                  {t('admin.deployments.settings.reset')}
                </Button>
                <Button
                  onClick={requestSave}
                  disabled={!dirty || writeDisabled}
                >
                  {saving
                    ? t('admin.deployments.common.saving')
                    : t('admin.deployments.settings.save')}
                </Button>
              </div>
            ) : (
              <p className="text-meta text-ink-3">{t('admin.deployments.common.read_only')}</p>
            )}
          </div>
        ) : null}
      </SectionCard>

      <ConfirmDialog
        open={confirm !== null}
        title={
          confirm === 'clear'
            ? t('admin.deployments.settings.clear_confirm_title')
            : t('admin.deployments.settings.disable_confirm_title')
        }
        description={
          confirm === 'clear'
            ? t('admin.deployments.settings.clear_confirm_body')
            : t('admin.deployments.settings.disable_confirm_body')
        }
        confirmLabel={
          confirm === 'clear'
            ? t('admin.deployments.secret.clear')
            : t('admin.deployments.settings.save')
        }
        onCancel={() => setConfirm(null)}
        onConfirm={() => {
          const action = confirm
          setConfirm(null)
          if (action === 'clear') {
            setApiKey(null)
            setSavedNotice(false)
            return
          }
          void save()
        }}
      />
    </section>
  )
}
