import { useEffect, useId, useState, type KeyboardEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { discountFold } from '@/features/logs/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert, TextField } from '@/shared/ui/form'
import { Tag } from '@/shared/ui/tag'
import { useToastStore } from '@/shared/ui/toast'

type DiscountRule = components['schemas']['DiscountRule']
type DiscountRuleInput = components['schemas']['DiscountRuleInput']
type DiscountRulePatch = components['schemas']['DiscountRulePatch']

export type DiscountTargetType = 'user' | 'group'

export interface DiscountRuleTableProps {
  /** Discounts hang off either a user id or a group name. */
  targetType: DiscountTargetType
  targetID: string
}

interface FieldErrors {
  model?: string
  rate?: string
  until?: string
}

function rulesQueryKey(targetType: DiscountTargetType, targetID: string) {
  return ['discount-rules', targetType, targetID] as const
}

function modelsQueryKey(targetType: DiscountTargetType, q: string) {
  // `q` must be part of the key: React Query only re-runs `queryFn` when the
  // key changes (or an observer remounts), so a closure-only query never fires.
  return ['discount-models', targetType, q] as const
}

async function fetchRules(
  targetType: DiscountTargetType,
  targetID: string,
): Promise<DiscountRule[]> {
  if (targetType === 'user') {
    const data = requireData(
      await apiClient.GET('/api/users/{id}/discounts', {
        params: { path: { id: Number(targetID) } },
      }),
    )
    return data.items
  }
  const data = requireData(
    await apiClient.GET('/api/groups/{name}/discounts', {
      params: { path: { name: targetID } },
    }),
  )
  return data.items
}

async function fetchModels(
  targetType: DiscountTargetType,
  q: string,
): Promise<string[]> {
  const query = q === '' ? {} : { q }
  if (targetType === 'user') {
    const data = requireData(
      await apiClient.GET('/api/users/discount-models', {
        params: { query },
      }),
    )
    return data.items.map((item) => item.model_name)
  }
  const data = requireData(
    await apiClient.GET('/api/groups/discount-models', {
      params: { query },
    }),
  )
  return data.items.map((item) => item.model_name)
}

/** RFC3339 → the `YYYY-MM-DDTHH:mm` shape `<input type="datetime-local">` wants. */
export function toDatetimeLocal(value: string | null): string {
  if (value === null) return ''
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  const pad = (part: number): string => String(part).padStart(2, '0')
  // Local components, never `slice()`: the picker interprets its value as local
  // time, so formatting the local clock makes this the exact inverse of
  // `new Date(localValue)` and keeps edits round-trip safe in every timezone.
  // Minute precision only — the picker has no seconds field.
  return [
    `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`,
    `${pad(date.getHours())}:${pad(date.getMinutes())}`,
  ].join('T')
}

/** Picker value (local time) → RFC3339 UTC, the inverse of `toDatetimeLocal`. */
function fromDatetimeLocal(value: string): string | null {
  if (value === '') return null
  return new Date(value).toISOString()
}

/** Display date only, in the same local calendar the picker uses. */
function formatRuleDate(value: string | null): string {
  if (value === null) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '—'
  const pad = (part: number): string => String(part).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
}

/**
 * Target-agnostic discount rule table. It owns its own queries/mutations so the
 * user sheet and the group sheet (Task 13) share one implementation; the only
 * difference is the endpoint prefix derived from `targetType`.
 */
export function DiscountRuleTable({ targetType, targetID }: DiscountRuleTableProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const modelListId = useId()

  const [formOpen, setFormOpen] = useState(false)
  const [editingRule, setEditingRule] = useState<DiscountRule | null>(null)
  const [model, setModel] = useState('')
  const [rate, setRate] = useState('')
  const [validFrom, setValidFrom] = useState('')
  const [validUntil, setValidUntil] = useState('')
  const [enabled, setEnabled] = useState(true)
  const [modelSearch, setModelSearch] = useState('')
  const [modelQuery, setModelQuery] = useState('')
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>({})
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)
  const [deletingRule, setDeletingRule] = useState<DiscountRule | null>(null)
  const [deleteError, setDeleteError] = useState<string | undefined>(undefined)

  // Debounced model search: the picker endpoint takes a substring query.
  useEffect(() => {
    const timer = setTimeout(() => {
      setModelQuery(modelSearch.trim())
    }, 300)
    return () => {
      clearTimeout(timer)
    }
  }, [modelSearch])

  const rulesQuery = useQuery({
    queryKey: rulesQueryKey(targetType, targetID),
    queryFn: () => fetchRules(targetType, targetID),
  })

  const modelsQuery = useQuery({
    queryKey: modelsQueryKey(targetType, modelQuery),
    queryFn: () => fetchModels(targetType, modelQuery),
  })

  const createMutation = useMutation({
    mutationFn: async (input: DiscountRuleInput) => {
      if (targetType === 'user') {
        await apiClient.POST('/api/users/{id}/discounts', {
          params: { path: { id: Number(targetID) } },
          body: input,
        })
        return
      }
      await apiClient.POST('/api/groups/{name}/discounts', {
        params: { path: { name: targetID } },
        body: input,
      })
    },
  })

  const updateMutation = useMutation({
    mutationFn: async ({
      id,
      patch,
    }: {
      id: number
      patch: DiscountRulePatch
    }) => {
      if (targetType === 'user') {
        await apiClient.PATCH('/api/users/{id}/discounts/{discount_id}', {
          params: { path: { id: Number(targetID), discount_id: id } },
          body: patch,
        })
        return
      }
      await apiClient.PATCH('/api/groups/{name}/discounts/{discount_id}', {
        params: { path: { name: targetID, discount_id: id } },
        body: patch,
      })
    },
  })

  const deleteMutation = useMutation({
    mutationFn: async (id: number) => {
      if (targetType === 'user') {
        await apiClient.DELETE('/api/users/{id}/discounts/{discount_id}', {
          params: { path: { id: Number(targetID), discount_id: id } },
        })
        return
      }
      await apiClient.DELETE('/api/groups/{name}/discounts/{discount_id}', {
        params: { path: { name: targetID, discount_id: id } },
      })
    },
  })

  const rules = rulesQuery.data ?? []
  const models = modelsQuery.data ?? []
  const modelsError = modelsQuery.error
  const modelsForbidden =
    modelsError instanceof ApiError && modelsError.status === 403

  async function invalidate(): Promise<void> {
    await queryClient.invalidateQueries({
      queryKey: rulesQueryKey(targetType, targetID),
    })
    // The user row may summarise discounts; keep that list honest after a write.
    await queryClient.invalidateQueries({ queryKey: ['admin-users'] })
  }

  function resetForm(): void {
    setFormOpen(false)
    setEditingRule(null)
    setModel('')
    setModelSearch('')
    setRate('')
    setValidFrom('')
    setValidUntil('')
    setEnabled(true)
    setFieldErrors({})
    setFormError(undefined)
  }

  function startCreate(): void {
    resetForm()
    setFormOpen(true)
  }

  function startEdit(rule: DiscountRule): void {
    resetForm()
    setEditingRule(rule)
    setModel(rule.model_name)
    setRate(String(rule.rate_percent))
    setValidFrom(toDatetimeLocal(rule.valid_from))
    setValidUntil(toDatetimeLocal(rule.valid_until))
    setEnabled(rule.enabled)
    setFormOpen(true)
  }

  /** `0 → Free`, `100 → full price`, otherwise the fold number for the UI. */
  function rateLabel(value: number): string | undefined {
    if (!Number.isInteger(value) || value < 0 || value > 100) return undefined
    if (value === 0) return t('admin.user_form.discount_rate_free')
    if (value === 100) return t('admin.user_form.discount_rate_full')
    return t('admin.user_form.discount_rate_fold', {
      value: discountFold(value / 100),
    })
  }

  async function submitRule(): Promise<void> {
    const errors: FieldErrors = {}
    if (editingRule === null && model.trim() === '') {
      errors.model = t('admin.user_form.discount_model_required')
    }
    const rateText = rate.trim()
    const rateNumber = Number(rateText)
    if (!/^\d+$/.test(rateText) || rateNumber < 0 || rateNumber > 100) {
      errors.rate = t('admin.user_form.discount_rate_invalid')
    }
    const from = fromDatetimeLocal(validFrom)
    const until = fromDatetimeLocal(validUntil)
    if (
      validFrom !== '' &&
      validUntil !== '' &&
      new Date(validFrom) >= new Date(validUntil)
    ) {
      errors.until = t('admin.user_form.discount_window_invalid')
    }
    setFieldErrors(errors)
    if (Object.keys(errors).length > 0) return

    setFormError(undefined)
    setSubmitting(true)
    try {
      if (editingRule === null) {
        await createMutation.mutateAsync({
          model_name: model.trim(),
          rate_percent: rateNumber,
          valid_from: from,
          valid_until: until,
          enabled,
        })
      } else {
        // `model_name` must never appear in a PATCH body: the server answers
        // 422 `model_immutable` when the key is present at all.
        await updateMutation.mutateAsync({
          id: editingRule.id,
          patch: {
            rate_percent: rateNumber,
            valid_from: from,
            valid_until: until,
            enabled,
          },
        })
      }
      push({ tone: 'success', message: t('admin.user_form.discount_saved') })
      resetForm()
      await invalidate()
    } catch {
      setFormError(t('admin.user_form.discount_save_error'))
    } finally {
      setSubmitting(false)
    }
  }

  async function confirmDelete(): Promise<void> {
    if (deletingRule === null) return
    setDeleteError(undefined)
    try {
      await deleteMutation.mutateAsync(deletingRule.id)
      push({ tone: 'success', message: t('admin.user_form.discount_deleted') })
      setDeletingRule(null)
      await invalidate()
    } catch {
      setDeleteError(t('admin.user_form.discount_delete_error'))
    }
  }

  // This editor lives inside the sheet's own <form>, so Enter on any input
  // must never fall through to the user form's implicit submission. Plain
  // free-text fields (rate / dates) also save the rule; the datalist-backed
  // model picker leaves Enter to the suggestion list, and the enabled checkbox
  // does not save anything — both are only stopped from bubbling into the
  // outer form.
  function handleFormKeyDown(event: KeyboardEvent<HTMLDivElement>): void {
    if (event.key !== 'Enter') return
    const target = event.target as HTMLElement
    if (target.tagName !== 'INPUT') return
    const input = target as HTMLInputElement
    const savesRule =
      (input.type === 'text' ||
        input.type === 'number' ||
        input.type === 'datetime-local') &&
      !input.hasAttribute('list')
    event.preventDefault()
    if (savesRule) void submitRule()
  }

  return (
    <div className="flex flex-col gap-3">
      {modelsQuery.isError ? (
        <FormAlert tone="warning">
          {modelsForbidden
            ? t('admin.user_form.discount_models_forbidden')
            : t('admin.user_form.discount_models_error')}
        </FormAlert>
      ) : null}

      {formOpen ? (
        <div
          className="flex flex-col gap-3 rounded-lg border border-border-hairline p-3"
          onKeyDown={handleFormKeyDown}
        >
          {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}
          <TextField
            label={t('admin.user_form.discount_model')}
            value={model}
            list={modelListId}
            disabled={editingRule !== null}
            hint={
              editingRule !== null
                ? t('admin.user_form.discount_model_locked')
                : undefined
            }
            error={fieldErrors.model}
            onChange={(event) => {
              setModel(event.target.value)
              setModelSearch(event.target.value)
              setFieldErrors((current) => ({ ...current, model: undefined }))
            }}
          />
          <datalist id={modelListId}>
            {models.map((option) => (
              <option key={option} value={option} />
            ))}
          </datalist>
          <TextField
            label={t('admin.user_form.discount_rate')}
            type="number"
            min={0}
            max={100}
            step={1}
            value={rate}
            error={fieldErrors.rate}
            onChange={(event) => {
              setRate(event.target.value)
              setFieldErrors((current) => ({ ...current, rate: undefined }))
            }}
          />
          {rate.trim() !== '' && rateLabel(Number(rate)) !== undefined ? (
            <p
              className={[
                'text-meta',
                Number(rate) === 100 ? 'text-warning' : 'text-ink-3',
              ].join(' ')}
            >
              {rateLabel(Number(rate))}
            </p>
          ) : null}
          <div className="grid gap-3 sm:grid-cols-2">
            <TextField
              label={t('admin.user_form.discount_valid_from')}
              type="datetime-local"
              value={validFrom}
              onChange={(event) => {
                setValidFrom(event.target.value)
                setFieldErrors((current) => ({ ...current, until: undefined }))
              }}
            />
            <TextField
              label={t('admin.user_form.discount_valid_until')}
              type="datetime-local"
              value={validUntil}
              error={fieldErrors.until}
              onChange={(event) => {
                setValidUntil(event.target.value)
                setFieldErrors((current) => ({ ...current, until: undefined }))
              }}
            />
          </div>
          <label className="flex items-center gap-2 text-body text-ink-1">
            <input
              type="checkbox"
              checked={enabled}
              onChange={(event) => {
                setEnabled(event.target.checked)
              }}
            />
            {t('admin.user_form.discount_enabled')}
          </label>
          <div className="flex justify-end gap-2">
            <Button variant="secondary" type="button" onClick={resetForm}>
              {t('common.action.cancel')}
            </Button>
            <Button
              type="button"
              disabled={submitting}
              onClick={() => {
                void submitRule()
              }}
            >
              {t('admin.user_form.discount_save')}
            </Button>
          </div>
        </div>
      ) : (
        <div className="flex justify-end">
          <Button variant="secondary" type="button" onClick={startCreate}>
            {t('admin.user_form.discount_add')}
          </Button>
        </div>
      )}

      {rulesQuery.isPending ? (
        <div className="space-y-2" aria-busy="true">
          {[0, 1, 2].map((index) => (
            <div key={index} className="h-9 animate-pulse rounded bg-surface-subtle" />
          ))}
        </div>
      ) : rulesQuery.isError ? (
        <FormAlert>{t('admin.user_form.discount_error')}</FormAlert>
      ) : rules.length === 0 ? (
        <p className="text-body text-ink-2">
          {t('admin.user_form.discount_empty')}
        </p>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[640px] text-body">
            <thead>
              <tr className="border-b border-border-hairline text-left text-meta text-ink-2">
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.user_form.discount_model')}
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.user_form.discount_rate')}
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.user_form.discount_window')}
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.user_form.discount_status')}
                </th>
                <th scope="col" className="px-3 py-2 text-right font-medium">
                  {t('admin.user_form.discount_actions')}
                </th>
              </tr>
            </thead>
            <tbody>
              {rules.map((rule) => (
                <tr
                  key={rule.id}
                  className="border-b border-border-hairline last:border-0"
                >
                  <td className="px-3 py-2">
                    <span className="font-mono text-ink-1">{rule.model_name}</span>
                  </td>
                  <td className="px-3 py-2 text-ink-2">
                    {rateLabel(rule.rate_percent)}
                  </td>
                  <td className="px-3 py-2 font-mono tabular-nums text-ink-2">
                    {`${formatRuleDate(rule.valid_from)} ~ ${formatRuleDate(rule.valid_until)}`}
                  </td>
                  <td className="px-3 py-2">
                    <Tag variant={rule.enabled ? 'success' : 'neutral'}>
                      {rule.enabled
                        ? t('admin.user_form.discount_status_on')
                        : t('admin.user_form.discount_status_off')}
                    </Tag>
                  </td>
                  <td className="px-3 py-2">
                    <div className="flex justify-end gap-1">
                      <Button
                        variant="ghost"
                        type="button"
                        className="h-8 px-2 text-meta"
                        onClick={() => {
                          startEdit(rule)
                        }}
                      >
                        {t('admin.user_form.discount_edit')}
                      </Button>
                      <Button
                        variant="ghost"
                        type="button"
                        className="h-8 px-2 text-meta text-danger hover:bg-danger-bg"
                        onClick={() => {
                          setDeleteError(undefined)
                          setDeletingRule(rule)
                        }}
                      >
                        {t('admin.user_form.discount_delete')}
                      </Button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <Dialog
        open={deletingRule !== null}
        onOpenChange={(open) => {
          if (!open) {
            setDeletingRule(null)
            setDeleteError(undefined)
          }
        }}
        title={t('admin.user_form.discount_delete_title')}
        description={
          deletingRule === null
            ? undefined
            : t('admin.user_form.discount_delete_body', {
                model: deletingRule.model_name,
              })
        }
        footer={
          <>
            <Button
              variant="secondary"
              type="button"
              onClick={() => setDeletingRule(null)}
            >
              {t('common.action.cancel')}
            </Button>
            <Button
              variant="danger"
              type="button"
              disabled={deleteMutation.isPending}
              onClick={() => {
                void confirmDelete()
              }}
            >
              {t('admin.user_form.discount_delete_confirm')}
            </Button>
          </>
        }
      >
        {deleteError !== undefined ? (
          <FormAlert>{deleteError}</FormAlert>
        ) : null}
      </Dialog>
    </div>
  )
}
