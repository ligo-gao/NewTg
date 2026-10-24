import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert, TextField } from '@/shared/ui/form'

import {
  createIdempotencyKey,
  fetchDeployment,
  updateDeployment,
  type UpdateDeploymentRequest,
} from './api'
import { ConfirmDialog, EnvEditor } from './components'
import type { DeploymentActionTarget } from './deployments-tab'
import { SecretField } from './secret-field'
import {
  envToRows,
  errorMessage,
  isUnknownResult,
  retryAfterOf,
  rowsToEnv,
  splitCommand,
  type EnvRow,
} from './utils'

/**
 * 更新配置（F-08-57）。秘密类字段留空 = 不更新；显式清除用
 * `clear_registry_secret` / `clear_secret_env`，且必须再过一次确认。
 * 提交前展示「可能重启容器」警告。
 */
export function UpdateDialog({
  target,
  onClose,
}: {
  target: DeploymentActionTarget | null
  onClose: () => void
}) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const open = target !== null
  const detailQuery = useQuery({
    queryKey: ['deployment', target?.id],
    queryFn: ({ signal }) => fetchDeployment(target?.id ?? '', signal),
    enabled: open,
  })
  const detail = detailQuery.data

  const [imageUrl, setImageUrl] = useState('')
  const [registryUsername, setRegistryUsername] = useState('')
  const [registrySecret, setRegistrySecret] = useState<string | null>('')
  const [clearRegistrySecret, setClearRegistrySecret] = useState(false)
  const [entrypoint, setEntrypoint] = useState('')
  const [args, setArgs] = useState('')
  const [command, setCommand] = useState('')
  const [trafficPort, setTrafficPort] = useState('')
  const [envRows, setEnvRows] = useState<EnvRow[]>([])
  const [secretEnvRows, setSecretEnvRows] = useState<EnvRow[]>([])
  const [clearSecretEnv, setClearSecretEnv] = useState(false)

  const [confirmOpen, setConfirmOpen] = useState(false)
  const [pendingClear, setPendingClear] = useState<'registry' | 'env' | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<unknown>(null)
  // 504 后幂等键已释放：禁止同 key 重提，只能关闭刷新后用新会话重做。
  const [unknownResult, setUnknownResult] = useState(false)
  const [idempotencyKey, setIdempotencyKey] = useState<string | null>(null)

  // 详情到达后初始化表单；秘密字段永远从空开始（不回显）。
  useEffect(() => {
    if (detail === undefined) return
    const config = detail.container_config
    setImageUrl(config?.image_url ?? '')
    setEntrypoint((config?.entrypoint ?? []).join(' '))
    setEnvRows(envToRows(config?.env_variables))
    setArgs('')
    setCommand('')
    setTrafficPort(config?.traffic_port === undefined ? '' : String(config.traffic_port))
    setRegistryUsername('')
    setRegistrySecret('')
    setClearRegistrySecret(false)
    setClearSecretEnv(false)
    setSecretEnvRows([])
  }, [detail, open])

  const close = (): void => {
    setError(null)
    setSubmitting(false)
    setConfirmOpen(false)
    setPendingClear(null)
    setUnknownResult(false)
    setIdempotencyKey(null)
    // 关闭即丢弃草稿：同一 detail 缓存引用下重新打开不应保留上次编辑。
    setImageUrl('')
    setRegistryUsername('')
    setRegistrySecret('')
    setClearRegistrySecret(false)
    setEntrypoint('')
    setArgs('')
    setCommand('')
    setTrafficPort('')
    setEnvRows([])
    setSecretEnvRows([])
    setClearSecretEnv(false)
    onClose()
  }

  const buildBody = (): UpdateDeploymentRequest => {
    const body: UpdateDeploymentRequest = {}
    const original = detail?.container_config
    if (imageUrl.trim() !== '' && imageUrl.trim() !== (original?.image_url ?? '')) {
      body.image_url = imageUrl.trim()
    }
    if (registryUsername.trim() !== '') body.registry_username = registryUsername.trim()
    if (registrySecret !== null && registrySecret !== '') body.registry_secret = registrySecret
    if (clearRegistrySecret) body.clear_registry_secret = true
    if (entrypoint.trim() !== '') body.entrypoint = splitCommand(entrypoint)
    if (args.trim() !== '') body.args = splitCommand(args)
    if (command.trim() !== '') body.command = command.trim()
    if (trafficPort.trim() !== '') {
      const port = Number(trafficPort)
      if (Number.isInteger(port) && port > 0) body.traffic_port = port
    }
    body.env_variables = rowsToEnv(envRows)
    const secretValues = rowsToEnv(secretEnvRows)
    if (Object.keys(secretValues).length > 0) body.secret_env_variables = secretValues
    if (clearSecretEnv) body.clear_secret_env = true
    return body
  }

  const submit = async (): Promise<void> => {
    if (target === null || submitting || unknownResult) return
    const key = idempotencyKey ?? createIdempotencyKey()
    if (idempotencyKey === null) setIdempotencyKey(key)
    setSubmitting(true)
    setError(null)
    try {
      await updateDeployment(target.id, buildBody(), key)
      await queryClient.invalidateQueries({ queryKey: ['deployments'] })
      await queryClient.invalidateQueries({ queryKey: ['deployment', target.id] })
      close()
    } catch (cause) {
      setConfirmOpen(false)
      if (isUnknownResult(cause)) setUnknownResult(true)
      setError(cause)
    } finally {
      setSubmitting(false)
    }
  }

  const retryAfter = retryAfterOf(error)

  return (
    <>
      <Dialog
        open={open}
        onOpenChange={(next) => {
          if (!next) close()
        }}
        title={t('admin.deployments.update.title')}
        description={target?.name}
        variant="sheet"
        panelClassName="max-w-[720px]"
        footer={
          <>
            <Button variant="secondary" onClick={close} disabled={submitting}>
              {t('admin.deployments.confirm.cancel')}
            </Button>
            <Button
              onClick={() => setConfirmOpen(true)}
              disabled={submitting || unknownResult || detail === undefined}
            >
              {submitting
                ? t('admin.deployments.common.saving')
                : t('admin.deployments.update.save')}
            </Button>
          </>
        }
      >
        <div className="space-y-4">
          {detailQuery.isPending ? (
            <p className="text-body text-ink-2" role="status">
              {t('admin.deployments.common.loading')}
            </p>
          ) : null}
          {detailQuery.isError ? (
            <div className="space-y-2">
              <FormAlert tone="danger">{errorMessage(detailQuery.error)}</FormAlert>
              <Button variant="secondary" onClick={() => void detailQuery.refetch()}>
                {t('admin.deployments.common.retry')}
              </Button>
            </div>
          ) : null}
          {detail !== undefined ? (
            <>
              <TextField
                label={t('admin.deployments.update.image_url')}
                value={imageUrl}
                onChange={(event) => setImageUrl(event.target.value)}
              />
              <TextField
                label={t('admin.deployments.update.registry_username')}
                value={registryUsername}
                onChange={(event) => setRegistryUsername(event.target.value)}
              />
              <SecretField
                id="deploy-update-registry-secret"
                label={t('admin.deployments.update.registry_secret')}
                value={registrySecret}
                showBadge={false}
                hint={t('admin.deployments.update.secret_blank_hint')}
                onChange={(value) => {
                  setRegistrySecret(value)
                  setClearRegistrySecret(false)
                }}
              />
              <label className="flex items-center gap-2 text-body text-ink-2">
                <input
                  type="checkbox"
                  checked={clearRegistrySecret}
                  onChange={(event) => {
                    if (event.target.checked) setPendingClear('registry')
                    else setClearRegistrySecret(false)
                  }}
                />
                {t('admin.deployments.update.clear_registry_secret')}
              </label>
              <TextField
                label={t('admin.deployments.update.entrypoint')}
                value={entrypoint}
                onChange={(event) => setEntrypoint(event.target.value)}
              />
              <TextField
                label={t('admin.deployments.update.args')}
                value={args}
                onChange={(event) => setArgs(event.target.value)}
              />
              <TextField
                label={t('admin.deployments.update.command')}
                value={command}
                onChange={(event) => setCommand(event.target.value)}
              />
              <TextField
                label={t('admin.deployments.update.traffic_port')}
                type="number"
                inputMode="numeric"
                min={1}
                value={trafficPort}
                onChange={(event) => setTrafficPort(event.target.value)}
              />
              <EnvEditor
                idPrefix="deploy-update-env"
                label={t('admin.deployments.update.env')}
                rows={envRows}
                onChange={setEnvRows}
              />
              <EnvEditor
                idPrefix="deploy-update-secret-env"
                label={t('admin.deployments.update.secret_env')}
                rows={secretEnvRows}
                onChange={setSecretEnvRows}
                secret
                hint={t('admin.deployments.update.secret_blank_hint')}
              />
              <label className="flex items-center gap-2 text-body text-ink-2">
                <input
                  type="checkbox"
                  checked={clearSecretEnv}
                  onChange={(event) => {
                    if (event.target.checked) setPendingClear('env')
                    else setClearSecretEnv(false)
                  }}
                />
                {t('admin.deployments.update.clear_secret_env')}
              </label>
            </>
          ) : null}
          {error !== null ? (
            <FormAlert tone="danger">
              {isUnknownResult(error)
                ? t('admin.deployments.common.unknown_result')
                : errorMessage(error)}
              {retryAfter !== undefined
                ? ` ${t('admin.deployments.settings.retry_after', { seconds: retryAfter })}`
                : ''}
            </FormAlert>
          ) : null}
          {unknownResult && target !== null ? (
            <Button
              variant="secondary"
              onClick={() => {
                const id = target.id
                close()
                void queryClient.invalidateQueries({ queryKey: ['deployments'] })
                void queryClient.invalidateQueries({ queryKey: ['deployment', id] })
              }}
            >
              {t('admin.deployments.common.close_and_refresh')}
            </Button>
          ) : null}
        </div>
      </Dialog>

      <ConfirmDialog
        open={confirmOpen}
        title={t('admin.deployments.update.title')}
        description={t('admin.deployments.update.restart_warning')}
        confirmLabel={t('admin.deployments.update.save')}
        onCancel={() => setConfirmOpen(false)}
        onConfirm={() => void submit()}
        dangerous={false}
      />

      <ConfirmDialog
        open={pendingClear !== null}
        title={t('admin.deployments.update.clear_confirm_title')}
        description={t('admin.deployments.update.clear_confirm_body')}
        confirmLabel={t('admin.deployments.secret.clear')}
        onCancel={() => setPendingClear(null)}
        onConfirm={() => {
          if (pendingClear === 'registry') setClearRegistrySecret(true)
          if (pendingClear === 'env') setClearSecretEnv(true)
          setPendingClear(null)
        }}
      />
    </>
  )
}
