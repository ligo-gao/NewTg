import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { useForm } from 'react-hook-form'
import { z } from 'zod'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { TextField } from '@/shared/ui/form'
import { pushErrorToast, useToastStore } from '@/shared/ui/toast'

import {
  MAX_QUANTITY,
  MASKED_KEY,
  createTokens,
  getToken,
  listAutoGroups,
  resetTokenKey,
  updateToken,
  type CreateTokenRequest,
  type Token,
  type UpdateTokenRequest,
} from './api'
import { AutoGroupEditor } from './auto-group-editor'
import { CcSwitchDialog } from './cc-switch-dialog'
import { KeyRevealDialog, type RevealedKey } from './key-reveal-dialog'

const EXPIRATIONS = ['never', 'hour', 'day', 'month', 'custom'] as const
type Expiration = (typeof EXPIRATIONS)[number]

interface FormValues {
  name: string
  quantity: string
  group: string
  expiration: Expiration
  customExpiration: string
  unlimited: boolean
  quota: string
  modelLimits: string
  allowIps: string
  autoGroups: string[]
  crossGroupRetry: boolean
}

const DEFAULTS: FormValues = {
  name: '',
  quantity: '1',
  group: '',
  expiration: 'never',
  customExpiration: '',
  unlimited: true,
  quota: '',
  modelLimits: '',
  allowIps: '',
  autoGroups: [],
  crossGroupRetry: false,
}

/** RFC 7807 `fields` are wire names; translate them to form field names. */
const FORM_FIELD_BY_API: Record<string, keyof FormValues> = {
  name: 'name',
  quantity: 'quantity',
  group: 'group',
  expires_at: 'customExpiration',
  remain_quota: 'quota',
  model_limits: 'modelLimits',
  allow_ips: 'allowIps',
  auto_groups: 'autoGroups',
  cross_group_retry: 'crossGroupRetry',
}

function splitList(raw: string): string[] {
  return raw
    .split(/[\s,]+/)
    .map((item) => item.trim())
    .filter((item) => item !== '')
}

/**
 * IPv4/IPv6 literal or CIDR, validated without DNS. Kept local so the form can
 * reject bad entries before the request (modules-02 §2.4) — the server still
 * validates authoritatively.
 */
export function isValidIpOrCidr(raw: string): boolean {
  const [address, prefix] = raw.split('/')
  if (address === undefined || address === '') return false
  const isV4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.test(address)
  const isV6 = address.includes(':') && /^[0-9a-fA-F:.]+$/.test(address)
  if (!isV4 && !isV6) return false
  if (!isV4 && address.includes(':::')) return false
  if (prefix !== undefined) {
    const max = isV4 ? 32 : 128
    if (!/^\d{1,3}$/.test(prefix)) return false
    const value = Number(prefix)
    if (value < 0 || value > max) return false
  }
  if (isV4) {
    return address.split('.').every((part) => Number(part) <= 255)
  }
  return true
}

const formSchema = z
  .object({
    name: z
      .string()
      .trim()
      .min(1, 'common.tokens.form.nameRequired')
      .max(64, 'common.tokens.form.nameTooLong'),
    quantity: z.coerce
      .number()
      .int()
      .min(1, 'common.tokens.form.quantityRange')
      .max(MAX_QUANTITY, 'common.tokens.form.quantityRange'),
    // Empty means "server falls back to the user's current group" on create.
    // Edit requires a concrete value (checked in submit).
    group: z.string(),
    expiration: z.enum(EXPIRATIONS),
    customExpiration: z.string(),
    unlimited: z.boolean(),
    quota: z.string(),
    modelLimits: z.string(),
    allowIps: z.string(),
    autoGroups: z.array(z.string()),
    crossGroupRetry: z.boolean(),
  })
  .superRefine((values, ctx) => {
    // Custom expiry is validated in `submit`, where the edit sheet can tell a
    // newly chosen past value from an unchanged stored one (an expired token
    // must still be editable — e.g. rename or quota change).
    if (!values.unlimited) {
      const raw = values.quota.trim()
      if (!/^\d+$/.test(raw)) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          path: ['quota'],
          message: 'common.tokens.form.quotaInvalid',
        })
      } else if (Number(raw) > 1_000_000_000_000_000) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          path: ['quota'],
          message: 'common.tokens.form.quotaRange',
        })
      }
    }
    if (values.group === 'auto' && values.autoGroups.length === 0) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        path: ['autoGroups'],
        message: 'common.tokens.autoGroups.minOne',
      })
    }
    for (const ip of splitList(values.allowIps)) {
      if (!isValidIpOrCidr(ip)) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          path: ['allowIps'],
          message: 'common.tokens.form.invalidIp',
        })
        break
      }
    }
  })

function expirationToIso(values: {
  expiration: Expiration
  customExpiration: string
}): string | null {
  const now = Date.now()
  if (values.expiration === 'never') return null
  if (values.expiration === 'custom') {
    return new Date(values.customExpiration).toISOString()
  }
  const duration =
    values.expiration === 'hour'
      ? 3_600_000
      : values.expiration === 'day'
        ? 86_400_000
        : 30 * 86_400_000
  return new Date(now + duration).toISOString()
}

/**
 * `datetime-local` requires a local `YYYY-MM-DDTHH:mm` value. Passing the raw
 * ISO timestamp (with `Z`) leaves the input blank and would silently clear the
 * expiry on save.
 */
function toLocalDateTimeInput(iso: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return ''
  const pad = (value: number): string => String(value).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(
    date.getDate(),
  )}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

function tokenToForm(token: Token): FormValues {
  return {
    name: token.name,
    quantity: '1',
    group: token.group,
    // Existing expiry must round-trip through the custom field; defaulting to
    // `never` would erase it when the user only renames the token.
    expiration: token.expires_at ? 'custom' : 'never',
    customExpiration:
      token.expires_at === null || token.expires_at === undefined
        ? ''
        : toLocalDateTimeInput(token.expires_at),
    unlimited: token.unlimited_quota,
    quota:
      token.remain_quota === null || token.remain_quota === undefined
        ? ''
        : String(token.remain_quota),
    modelLimits: (token.model_limits ?? []).join(', '),
    allowIps: (token.allow_ips ?? []).join(', '),
    autoGroups: token.auto_groups ?? [],
    crossGroupRetry: token.cross_group_retry,
  }
}

export interface TokenMutateSheetProps {
  open: boolean
  mode: 'create' | 'edit'
  tokenId?: number | undefined
  onOpenChange: (open: boolean) => void
  onSaved: () => void
}

/**
 * Create/edit token sheet (modules-02 §2.4/§2.5, F-02-04/F-02-05/F-02-06).
 *
 * One component for both modes, matching the spec: edit adds the read-only key
 * row, quota-override copy and optimistic-lock error branch. Validation is
 * declared once in `formSchema`; server field errors are folded back into the
 * same `setError` path so both layers surface identically.
 */
export function TokenMutateSheet({
  open,
  mode,
  tokenId,
  onOpenChange,
  onSaved,
}: TokenMutateSheetProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const formId = useId()
  const idempotencyKey = useRef<string>('')
  const [submitting, setSubmitting] = useState(false)
  // Create and edit have different 409 recovery: edit can refresh the row,
  // create has no row (and its detail query is disabled), so it must retry.
  const [conflict, setConflict] = useState<'create' | 'edit' | null>(null)
  const [resetOpen, setResetOpen] = useState(false)
  const [revealed, setRevealed] = useState<RevealedKey[]>([])
  const [ccSwitchKey, setCcSwitchKey] = useState<string | null>(null)

  const {
    register,
    handleSubmit,
    reset,
    setValue,
    setError,
    clearErrors,
    watch,
    formState: { errors },
  } = useForm<FormValues>({ defaultValues: DEFAULTS })

  const group = watch('group')
  const unlimited = watch('unlimited')
  const expiration = watch('expiration')
  const autoGroups = watch('autoGroups')

  const autoQuery = useQuery({
    queryKey: ['tokens', 'auto-groups'],
    queryFn: listAutoGroups,
    enabled: open,
  })

  const detailQuery = useQuery({
    queryKey: ['tokens', 'detail', tokenId],
    queryFn: () => getToken(tokenId as number),
    enabled: open && mode === 'edit' && tokenId !== undefined,
  })

  // Open/create resets every field; edit seeds from the detail response. The
  // idempotency key is minted once per sheet opening so retries replay.
  useEffect(() => {
    if (!open) return
    setConflict(null)
    setCcSwitchKey(null)
    idempotencyKey.current = crypto.randomUUID()
    if (mode === 'create') {
      reset(DEFAULTS)
    }
  }, [open, mode, reset])

  useEffect(() => {
    if (mode === 'edit' && detailQuery.data !== undefined) {
      reset(tokenToForm(detailQuery.data))
    }
  }, [mode, detailQuery.data, reset])

  const candidates = useMemo(() => autoQuery.data?.groups ?? [], [autoQuery.data])
  const maxCount = autoQuery.data?.max_count ?? 20

  const groupOptions = useMemo(() => {
    // Options come only from the server's auto-group candidates. The edit form
    // also keeps the token's current group selectable even if the candidate
    // set changed; nothing is invented client-side.
    const names = candidates.map((candidate) => candidate.name)
    if (detailQuery.data !== undefined) names.push(detailQuery.data.group)
    return Array.from(new Set(names.filter((name) => name !== 'auto')))
  }, [candidates, detailQuery.data])

  const originalGroup = detailQuery.data?.group

  function handleApiError(error: unknown): void {
    if (error instanceof ApiError) {
      if (error.kind === 'conflict') {
        setConflict(mode)
        return
      }
      const apiField = error.fields?.[0]
      if (error.kind === 'validation' && apiField !== undefined) {
        const field = FORM_FIELD_BY_API[apiField]
        if (field !== undefined) {
          setError(field, { message: error.detail ?? error.title })
          return
        }
      }
      pushErrorToast(error)
      return
    }
    useToastStore.getState().push({
      tone: 'error',
      message: t('common.toast.error'),
    })
  }

  async function submit(values: FormValues): Promise<void> {
    clearErrors()
    setConflict(null)
    const parsed = formSchema.safeParse(values)
    if (!parsed.success) {
      const seen = new Set<string>()
      for (const issue of parsed.error.issues) {
        const field = issue.path[0]
        if (typeof field !== 'string' || seen.has(field)) continue
        seen.add(field)
        setError(field as keyof FormValues, { message: t(issue.message) })
      }
      return
    }

    const parsedValues = parsed.data
    if (mode === 'edit' && parsedValues.group === '') {
      setError('group', { message: t('common.tokens.form.groupRequired') })
      return
    }
    if (parsedValues.expiration === 'custom') {
      const timestamp = Date.parse(parsedValues.customExpiration)
      // An expired token stays editable: an unchanged stored expiry is allowed,
      // but choosing a new past time is still rejected.
      const original = detailQuery.data?.expires_at ?? null
      const unchanged =
        original !== null &&
        toLocalDateTimeInput(original) === parsedValues.customExpiration
      if (Number.isNaN(timestamp) || (timestamp <= Date.now() && !unchanged)) {
        setError('customExpiration', {
          message: t('common.tokens.form.expirationPast'),
        })
        return
      }
    }
    const expiresAt = expirationToIso(parsedValues)
    const modelLimits = splitList(parsedValues.modelLimits)
    const allowIps = splitList(parsedValues.allowIps)
    const isAuto = parsedValues.group === 'auto'

    setSubmitting(true)
    try {
      if (mode === 'create') {
        const body: CreateTokenRequest = {
          name: parsedValues.name.trim(),
          quantity: parsedValues.quantity,
          unlimited_quota: parsedValues.unlimited,
          cross_group_retry: isAuto ? parsedValues.crossGroupRetry : false,
        }
        // Only send a group the user explicitly picked; an omitted group makes
        // the server fall back to the user's current group.
        if (parsedValues.group !== '') body.group = parsedValues.group
        if (expiresAt !== null) body.expires_at = expiresAt
        if (!parsedValues.unlimited) body.remain_quota = Number(parsedValues.quota)
        if (modelLimits.length > 0) body.model_limits = modelLimits
        if (allowIps.length > 0) body.allow_ips = allowIps
        if (isAuto) body.auto_groups = parsedValues.autoGroups

        const created = await createTokens(body, idempotencyKey.current)
        setRevealed(
          created.map((entry) => ({
            id: entry.id,
            name: entry.name,
            key: entry.key,
          })),
        )
        push({ tone: 'success', message: t('common.tokens.toast.created') })
      } else {
        const detail = detailQuery.data
        if (detail === undefined) return
        const body: UpdateTokenRequest = {
          name: parsedValues.name.trim(),
          group: parsedValues.group,
          expires_at: expiresAt,
          unlimited_quota: parsedValues.unlimited,
          model_limits: modelLimits,
          allow_ips: allowIps,
          cross_group_retry: isAuto ? parsedValues.crossGroupRetry : false,
          updated_at: detail.updated_at,
        }
        if (!parsedValues.unlimited) body.remain_quota = Number(parsedValues.quota)
        if (isAuto) body.auto_groups = parsedValues.autoGroups
        await updateToken(detail.id, body)
        push({ tone: 'success', message: t('common.tokens.toast.updated') })
      }
      onSaved()
      onOpenChange(false)
    } catch (error) {
      handleApiError(error)
    } finally {
      setSubmitting(false)
    }
  }

  async function confirmReset(): Promise<void> {
    if (tokenId === undefined) return
    setSubmitting(true)
    try {
      const result = await resetTokenKey(tokenId)
      setRevealed([
        {
          id: tokenId,
          name: detailQuery.data?.name ?? String(tokenId),
          key: result.key,
        },
      ])
      push({ tone: 'success', message: t('common.tokens.reset.done') })
      setResetOpen(false)
      onOpenChange(false)
      onSaved()
    } catch (error) {
      handleApiError(error)
    } finally {
      setSubmitting(false)
    }
  }

  const push = useToastStore((state) => state.push)
  const title = mode === 'create' ? t('common.tokens.form.createTitle') : t('common.tokens.form.editTitle')

  return (
    <>
      <Dialog
        open={open}
        onOpenChange={(next) => {
          if (!submitting) onOpenChange(next)
        }}
        title={title}
        dismissible={!submitting}
        variant="sheet"
        footer={
          <>
            <Button
              variant="secondary"
              disabled={submitting}
              onClick={() => {
                onOpenChange(false)
              }}
            >
              {t('common.tokens.form.cancel')}
            </Button>
            <Button
              type="submit"
              form={formId}
              disabled={
                submitting ||
                (mode === 'edit' && detailQuery.data === undefined) ||
                detailQuery.isError
              }
            >
              {submitting
                ? mode === 'create'
                  ? t('common.tokens.form.creating')
                  : t('common.tokens.form.saving')
                : mode === 'create'
                  ? t('common.tokens.form.create')
                  : t('common.tokens.form.save')}
            </Button>
          </>
        }
      >
        <form
          id={formId}
          noValidate
          onSubmit={(event) => {
            void handleSubmit(submit)(event)
          }}
          className="flex flex-col gap-5"
        >
          {conflict !== null ? (
            <div
              role="alert"
              className="flex items-center justify-between gap-2 rounded-md border border-danger bg-danger-bg px-3 py-2 text-xs text-danger"
            >
              <span>
                {conflict === 'create'
                  ? t('common.tokens.form.createConflict')
                  : t('common.tokens.conflict.body')}
              </span>
              {conflict === 'create' ? (
                <Button
                  variant="secondary"
                  onClick={() => {
                    setConflict(null)
                    void handleSubmit(submit)()
                  }}
                >
                  {t('common.error.retry')}
                </Button>
              ) : (
                <Button
                  variant="secondary"
                  onClick={() => {
                    setConflict(null)
                    void detailQuery.refetch()
                  }}
                >
                  {t('common.tokens.conflict.refresh')}
                </Button>
              )}
            </div>
          ) : null}

          {mode === 'edit' && detailQuery.isError ? (
            <div
              role="alert"
              className="flex items-center justify-between gap-2 rounded-md border border-danger bg-danger-bg px-3 py-2 text-xs text-danger"
            >
              <span>{t('common.tokens.error.load')}</span>
              <Button
                variant="secondary"
                onClick={() => {
                  void detailQuery.refetch()
                }}
              >
                {t('common.tokens.error.retry')}
              </Button>
            </div>
          ) : null}

          <section className="flex flex-col gap-3">
            <h3 className="text-base font-semibold text-ink-1">
              {t('common.tokens.form.basic')}
            </h3>
            <TextField
              label={t('common.tokens.form.name')}
              error={errors.name?.message}
              maxLength={64}
              {...register('name')}
            />
            {mode === 'edit' ? (
              <div className="flex flex-col gap-1">
                <span className="text-[13px] font-medium text-ink-1">
                  {t('common.tokens.form.key')}
                </span>
                <div className="flex items-center gap-2">
                  <code
                    aria-readonly="true"
                    aria-label={t('common.tokens.form.keyReadonly')}
                    className="flex-1 rounded-md bg-surface-subtle px-2 py-2 font-mono text-[13px] text-ink-1"
                  >
                    {MASKED_KEY}
                  </code>
                  <Button variant="secondary" onClick={() => setResetOpen(true)}>
                    {t('common.tokens.form.resetKey')}
                  </Button>
                </div>
              </div>
            ) : (
              <TextField
                type="number"
                min={1}
                max={MAX_QUANTITY}
                label={t('common.tokens.form.quantity')}
                error={errors.quantity?.message}
                {...register('quantity')}
              />
            )}
            <div className="flex flex-col gap-1.5">
              <label htmlFor="token-group" className="text-[13px] font-medium text-ink-1">
                {t('common.tokens.form.group')}
              </label>
              <select
                id="token-group"
                className="h-10 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
                {...register('group')}
              >
                <option value="">{t('common.tokens.form.selectGroup')}</option>
                {groupOptions.map((name) => (
                  <option key={name} value={name}>
                    {name}
                  </option>
                ))}
                <option value="auto" disabled={candidates.length === 0}>
                  {t('common.tokens.form.auto')}
                </option>
              </select>
              {errors.group?.message !== undefined ? (
                <p className="text-xs text-danger">{errors.group.message}</p>
              ) : null}
              {mode === 'edit' && originalGroup !== undefined && group !== originalGroup ? (
                <p className="text-xs text-warning">{t('common.tokens.form.groupSwitchHint')}</p>
              ) : null}
            </div>

            {group === 'auto' ? (
              <AutoGroupEditor
                candidates={candidates}
                maxCount={maxCount}
                value={autoGroups}
                error={errors.autoGroups?.message}
                onChange={(next) => {
                  setValue('autoGroups', next, { shouldValidate: false })
                }}
              />
            ) : null}

            <div className="flex flex-col gap-1.5">
              <label
                htmlFor="token-expiration"
                className="text-[13px] font-medium text-ink-1"
              >
                {t('common.tokens.form.expiration')}
              </label>
              <select
                id="token-expiration"
                className="h-10 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
                {...register('expiration')}
              >
                <option value="never">{t('common.tokens.form.never')}</option>
                <option value="hour">{t('common.tokens.form.hour')}</option>
                <option value="day">{t('common.tokens.form.day')}</option>
                <option value="month">{t('common.tokens.form.month')}</option>
                <option value="custom">{t('common.tokens.form.custom')}</option>
              </select>
            </div>
            {expiration === 'custom' ? (
              <TextField
                type="datetime-local"
                label={t('common.tokens.form.customExpiration')}
                error={errors.customExpiration?.message}
                {...register('customExpiration')}
              />
            ) : null}
          </section>

          <section className="flex flex-col gap-3 border-t border-border-hairline pt-4">
            <h3 className="text-base font-semibold text-ink-1">
              {t('common.tokens.form.quotaSection')}
            </h3>
            <label className="flex items-center gap-2 text-sm text-ink-1">
              <input type="checkbox" {...register('unlimited')} />
              {t('common.tokens.form.unlimited')}
            </label>
            {!unlimited ? (
              <TextField
                inputMode="numeric"
                label={t('common.tokens.form.quota')}
                hint={t('common.tokens.form.quotaOverride')}
                error={errors.quota?.message}
                {...register('quota')}
              />
            ) : null}
          </section>

          <details className="border-t border-border-hairline pt-4">
            <summary className="cursor-pointer text-base font-semibold text-ink-1">
              {t('common.tokens.form.advanced')}
            </summary>
            <div className="mt-3 flex flex-col gap-3">
              <TextField
                label={t('common.tokens.form.modelLimits')}
                hint={t('common.tokens.form.modelLimitsHint')}
                error={errors.modelLimits?.message}
                {...register('modelLimits')}
              />
              <TextField
                label={t('common.tokens.form.allowIps')}
                hint={t('common.tokens.form.allowIpsHint')}
                error={errors.allowIps?.message}
                {...register('allowIps')}
              />
              {group === 'auto' ? (
                <div className="flex flex-col gap-1">
                  <label className="flex items-center gap-2 text-sm text-ink-1">
                    <input type="checkbox" {...register('crossGroupRetry')} />
                    {t('common.tokens.form.crossGroupRetry')}
                  </label>
                  <p className="text-xs text-warning">
                    {t('common.tokens.form.crossGroupRetryHint')}
                  </p>
                </div>
              ) : null}
            </div>
          </details>
        </form>
      </Dialog>

      <Dialog
        open={resetOpen}
        onOpenChange={(next) => {
          if (!submitting) setResetOpen(next)
        }}
        title={t('common.tokens.reset.title')}
        description={t('common.tokens.reset.body')}
        dismissible={!submitting}
        footer={
          <>
            <Button
              variant="secondary"
              disabled={submitting}
              onClick={() => setResetOpen(false)}
            >
              {t('common.tokens.form.cancel')}
            </Button>
            <Button
              variant="danger"
              disabled={submitting}
              onClick={() => {
                void confirmReset()
              }}
            >
              {submitting ? t('common.tokens.reset.resetting') : t('common.tokens.reset.confirm')}
            </Button>
          </>
        }
      />

      <KeyRevealDialog
        entries={revealed}
        onImport={(entry) => {
          // Import removes ONLY this entry: a batch create's other plaintext
          // keys are unrecoverable from the server, so discarding the list
          // would lose them permanently.
          setRevealed((current) =>
            current.filter((item) => item.id !== entry.id),
          )
          setCcSwitchKey(entry.key)
        }}
        onClose={() => {
          setRevealed([])
          void queryClient.invalidateQueries({ queryKey: ['tokens'] })
        }}
      />

      <CcSwitchDialog
        open={ccSwitchKey !== null}
        plaintextKey={ccSwitchKey ?? ''}
        onOpenChange={(open) => {
          if (!open) setCcSwitchKey(null)
        }}
      />
    </>
  )
}
