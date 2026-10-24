import { useMutation } from '@tanstack/react-query'
import { useEffect, useState, type FormEvent } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Sheet } from '@/shared/ui/base/sheet'
import { Button } from '@/shared/ui/button'
import { FormAlert, TextField } from '@/shared/ui/form'

import {
  createAccount,
  createIdempotencyKey,
  updateAccount,
  type UpstreamAccount,
  type UpstreamAccountCreate,
  type UpstreamAccountUpdate,
} from './api'

export type AccountDialogMode = 'create' | 'edit'

export interface AccountDialogProps {
  open: boolean
  mode: AccountDialogMode
  /** 编辑时为列表里的那一行；新建时为 null。 */
  account: UpstreamAccount | null
  canManage: boolean
  onOpenChange: (open: boolean) => void
  onSaved: () => void
}

const FORM_ID = 'upstream-account-form'

/** 服务端缺省值；表单先填好，管理员可以直接改。 */
const DEFAULT_CURRENCY = 'CNY'
const DEFAULT_TIMEZONE = 'Asia/Shanghai'

function problemMessage(error: unknown): string {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : String(error)
}

/**
 * 上游账号新建/编辑抽屉（任务 16）。
 *
 * 复用与明细抽屉相同的 `Sheet` 原语。创建只提交管理员填写的字段（服务端
 * 补齐 currency/timezone 缺省）；编辑带 `expected_updated_at` 且只提交改动
 * 过的字段——两个缺省值不会被无谓地写回，清空 console_url 则显式提交
 * `null`，与契约的「缺省 = 不修改、null = 清空」一一对应。
 */
export function AccountDialog({
  open,
  mode,
  account,
  canManage,
  onOpenChange,
  onSaved,
}: AccountDialogProps) {
  const { t } = useI18n()
  const [name, setName] = useState('')
  const [provider, setProvider] = useState('')
  const [currency, setCurrency] = useState(DEFAULT_CURRENCY)
  const [timezone, setTimezone] = useState(DEFAULT_TIMEZONE)
  const [consoleUrl, setConsoleUrl] = useState('')
  const [note, setNote] = useState('')

  const saveMutation = useMutation({
    mutationFn: (payload: {
      body: UpstreamAccountCreate | UpstreamAccountUpdate
      idempotencyKey: string
    }) => {
      if (account === null) {
        return createAccount(
          payload.body as UpstreamAccountCreate,
          payload.idempotencyKey,
        )
      }
      return updateAccount(
        account.id,
        payload.body as UpstreamAccountUpdate,
        payload.idempotencyKey,
      )
    },
    retry: false,
    onSuccess: () => {
      onSaved()
      onOpenChange(false)
    },
  })
  const resetSave = saveMutation.reset
  const accountId = account?.id ?? null

  // 只在抽屉打开或换成另一个账号时播种。若把整个 `account` 放进依赖，保存
  // 成功后的列表重取会换掉对象身份，把管理员正在看的表单重置掉。
  useEffect(() => {
    if (!open) return
    setName(account?.name ?? '')
    setProvider(account?.provider ?? '')
    setCurrency(account?.currency ?? DEFAULT_CURRENCY)
    setTimezone(account?.timezone ?? DEFAULT_TIMEZONE)
    setConsoleUrl(account?.console_url ?? '')
    setNote(account?.note ?? '')
    resetSave()
    // eslint-disable-next-line react-hooks/exhaustive-deps -- 见上：只随开合与账号身份播种
  }, [open, accountId, resetSave])

  function submit(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault()
    if (!canManage || saveMutation.isPending) return
    const nextName = name.trim()
    const nextProvider = provider.trim()
    if (nextName === '' || nextProvider === '') return

    const nextCurrency = currency.trim()
    const nextTimezone = timezone.trim()
    const nextConsoleUrl = consoleUrl.trim()
    const nextNote = note.trim()
    const idempotencyKey = createIdempotencyKey()

    if (account === null) {
      const body: UpstreamAccountCreate = {
        name: nextName,
        provider: nextProvider,
      }
      if (nextCurrency !== '') body.currency = nextCurrency
      if (nextTimezone !== '') body.timezone = nextTimezone
      // 空串不是合法的绝对地址；留空即「未填写」，交给服务端用缺省。
      if (nextConsoleUrl !== '') body.console_url = nextConsoleUrl
      if (nextNote !== '') body.note = nextNote
      saveMutation.mutate({ body, idempotencyKey })
      return
    }

    const body: UpstreamAccountUpdate = {
      expected_updated_at: account.updated_at,
    }
    if (nextName !== account.name) body.name = nextName
    if (nextProvider !== account.provider) body.provider = nextProvider
    if (nextCurrency !== '' && nextCurrency !== account.currency) {
      body.currency = nextCurrency
    }
    if (nextTimezone !== '' && nextTimezone !== account.timezone) {
      body.timezone = nextTimezone
    }
    if (nextConsoleUrl !== (account.console_url ?? '')) {
      body.console_url = nextConsoleUrl === '' ? null : nextConsoleUrl
    }
    if (nextNote !== account.note) body.note = nextNote
    saveMutation.mutate({ body, idempotencyKey })
  }

  const busy = saveMutation.isPending
  const disabled = !canManage || busy
  const canSubmit =
    !disabled && name.trim() !== '' && provider.trim() !== ''

  return (
    <Sheet
      open={open}
      onOpenChange={onOpenChange}
      widthClassName="max-w-[520px]"
      title={t(
        mode === 'edit'
          ? 'admin.upstream_reconciliation.accounts.form_title_edit'
          : 'admin.upstream_reconciliation.accounts.form_title_create',
      )}
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            {t('common.action.close')}
          </Button>
          <Button type="submit" form={FORM_ID} disabled={!canSubmit}>
            {t(
              busy
                ? mode === 'edit'
                  ? 'admin.upstream_reconciliation.accounts.form.saving'
                  : 'admin.upstream_reconciliation.accounts.form.creating'
                : 'admin.upstream_reconciliation.accounts.form.save',
            )}
          </Button>
        </>
      }
    >
      <form id={FORM_ID} className="flex flex-col gap-3" onSubmit={submit}>
        <TextField
          label={t('admin.upstream_reconciliation.accounts.form.name')}
          value={name}
          maxLength={100}
          disabled={disabled}
          onChange={(event) => {
            setName(event.target.value)
          }}
        />
        <TextField
          label={t('admin.upstream_reconciliation.accounts.form.provider')}
          value={provider}
          maxLength={64}
          disabled={disabled}
          onChange={(event) => {
            setProvider(event.target.value)
          }}
        />
        <TextField
          label={t('admin.upstream_reconciliation.accounts.form.currency')}
          value={currency}
          maxLength={3}
          disabled={disabled}
          onChange={(event) => {
            setCurrency(event.target.value)
          }}
        />
        <TextField
          label={t('admin.upstream_reconciliation.accounts.form.timezone')}
          value={timezone}
          disabled={disabled}
          onChange={(event) => {
            setTimezone(event.target.value)
          }}
        />
        <TextField
          label={t('admin.upstream_reconciliation.accounts.form.console_url')}
          value={consoleUrl}
          inputMode="url"
          disabled={disabled}
          onChange={(event) => {
            setConsoleUrl(event.target.value)
          }}
        />
        <label
          htmlFor="upstream-account-note"
          className="flex flex-col gap-1.5 text-body font-medium text-ink-1"
        >
          {t('admin.upstream_reconciliation.accounts.form.note')}
          <textarea
            id="upstream-account-note"
            value={note}
            rows={3}
            maxLength={2000}
            disabled={disabled}
            onChange={(event) => {
              setNote(event.target.value)
            }}
            className="rounded-lg border border-border-default bg-surface-card px-3 py-2 text-body text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
          />
        </label>

        {saveMutation.isError ? (
          <FormAlert tone="danger">
            {problemMessage(saveMutation.error)}
          </FormAlert>
        ) : null}
      </form>
    </Sheet>
  )
}
