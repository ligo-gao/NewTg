import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'
import { useToastStore } from '@/shared/ui/toast'

type VendorOperationRequest = components['schemas']['VendorOperationRequest']
type VendorOperationPreview = components['schemas']['VendorOperationPreview']

export type VendorOperationAction = NonNullable<VendorOperationRequest['action']>

export interface VendorOperationsWizardProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** 列表勾选的供应商（merge/delete 的源集合）。 */
  selectedVendorIds: readonly number[]
  /** 「去 merge」快捷入口按此预选动作。 */
  initialAction?: VendorOperationAction
  onApplied: () => void
}

/**
 * F-06-24 供应商批量操作向导。
 *
 * **preview → apply 两段不可跳过**：apply 必须携带 preview 返回的
 * `expected_version`（操作指纹），缺失或不符服务端一律 409 并整体回滚。
 * 前端因此把 apply 按钮放在预览结果之后，并禁用「冲突」的预览——不是
 * 装饰性的步骤条。
 */
export function VendorOperationsWizard({
  open,
  onOpenChange,
  selectedVendorIds,
  initialAction = 'assign',
  onApplied,
}: VendorOperationsWizardProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const [action, setAction] = useState<VendorOperationAction>(initialAction)
  const [modelIds, setModelIds] = useState<ReadonlySet<number>>(new Set())
  const [modelKeyword, setModelKeyword] = useState('')
  const [vendorIds, setVendorIds] = useState<ReadonlySet<number>>(new Set())
  const [targetVendorId, setTargetVendorId] = useState('')
  const [preview, setPreview] = useState<VendorOperationPreview | null>(null)
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [applyError, setApplyError] = useState<string | undefined>(undefined)

  // 父级的选择集每次渲染都是新数组；用序列化键做 effect 依赖，避免向导
  // 在用户操作期间被「父级重渲染」重置掉已填的表单。
  const selectedKey = selectedVendorIds.join(',')

  useEffect(() => {
    if (!open) return
    const ids = selectedKey === '' ? [] : selectedKey.split(',').map(Number)
    setAction(initialAction)
    setModelIds(new Set())
    setModelKeyword('')
    setVendorIds(new Set(ids))
    setTargetVendorId('')
    setPreview(null)
    setFormError(undefined)
    setApplyError(undefined)
  }, [open, initialAction, selectedKey])

  const vendorsQuery = useQuery({
    queryKey: ['admin-vendor-options'],
    enabled: open,
    staleTime: 60_000,
    queryFn: async () => {
      return requireData(
        await apiClient.GET('/api/vendors/search', {
          params: { query: { limit: 100 } },
        }),
      )
    },
  })

  const modelsQuery = useQuery({
    queryKey: ['admin-model-options', modelKeyword],
    enabled: open && action === 'assign',
    queryFn: async () => {
      const query =
        modelKeyword.trim() === ''
          ? { limit: 50 }
          : { keyword: modelKeyword.trim(), limit: 50 }
      return requireData(
        await apiClient.GET('/api/models/search', { params: { query } }),
      )
    },
  })

  const vendorName = (id: number): string =>
    vendorsQuery.data?.items.find((vendor) => vendor.id === id)?.name ?? `#${id}`

  /**
   * 纯校验：返回可提交的预览/应用入参，或一条表单级错误。**不得**在这里
   * setState——render 阶段也会调用它来计算 apply 的可用性。
   */
  function validateRequest():
    | { body: VendorOperationRequest; error?: undefined }
    | { body: null; error: string } {
    if (action === 'assign') {
      if (modelIds.size === 0) {
        return { body: null, error: t('admin.vendors.ops_select_models') }
      }
      if (targetVendorId === '') {
        return { body: null, error: t('admin.vendors.ops_select_target') }
      }
      return {
        body: {
          action,
          model_ids: [...modelIds],
          target_vendor_id: Number(targetVendorId),
        },
      }
    }
    if (vendorIds.size === 0) {
      return { body: null, error: t('admin.vendors.ops_select_vendors') }
    }
    if (action === 'merge') {
      if (targetVendorId === '' || targetVendorId === '0') {
        return { body: null, error: t('admin.vendors.ops_select_target') }
      }
      if (vendorIds.has(Number(targetVendorId))) {
        return { body: null, error: t('admin.vendors.ops_target_in_sources') }
      }
      return {
        body: {
          action,
          vendor_ids: [...vendorIds],
          target_vendor_id: Number(targetVendorId),
        },
      }
    }
    return { body: { action, vendor_ids: [...vendorIds] } }
  }

  const runPreview = useMutation({
    mutationFn: async (body: VendorOperationRequest) => {
      return requireData(
        await apiClient.POST('/api/vendors/operations/preview', { body }),
      )
    },
    retry: false,
    onSuccess: (result) => {
      setPreview(result)
      setApplyError(undefined)
    },
    onError: (error) => {
      setPreview(null)
      setFormError(operationError(error, t))
    },
  })

  const apply = useMutation({
    mutationFn: async (input: {
      body: VendorOperationRequest
      expectedVersion: string
    }) => {
      return requireData(
        await apiClient.POST('/api/vendors/operations', {
          body: { ...input.body, expected_version: input.expectedVersion },
        }),
      )
    },
    retry: false,
    onSuccess: async (result) => {
      push({
        tone: 'success',
        message: t('admin.vendors.ops_success', { count: result.affected }),
      })
      await queryClient.invalidateQueries({ queryKey: ['admin-vendors'] })
      await queryClient.invalidateQueries({ queryKey: ['admin-models'] })
      await queryClient.invalidateQueries({ queryKey: ['admin-vendor-options'] })
      onApplied()
      onOpenChange(false)
    },
    onError: (error) => {
      if (error instanceof ApiError && error.kind === 'conflict') {
        // 指纹过期：预览已不是当前状态，必须重新预览（预览会刷出新的
        // expected_version 与冲突列表）。
        setApplyError(t('admin.vendors.ops_conflict'))
        setPreview(null)
        return
      }
      setApplyError(operationError(error, t))
    },
  })

  function previewNow(): void {
    const result = validateRequest()
    if (result.body === null) {
      setFormError(result.error)
      return
    }
    setFormError(undefined)
    setApplyError(undefined)
    runPreview.mutate(result.body)
  }

  const pending = preview === null ? null : validateRequest()
  const targetOptions = (vendorsQuery.data?.items ?? []).filter(
    (vendor) => !vendorIds.has(vendor.id),
  )

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      variant="sheet"
      panelClassName="!max-w-[640px] xl:!max-w-[720px]"
      title={t('admin.vendors.ops_title')}
      description={t('admin.vendors.ops_rollback_hint')}
    >
      <div className="flex flex-col gap-4">
        {formError !== undefined ? <FormAlert tone="danger">{formError}</FormAlert> : null}
        {applyError !== undefined ? (
          <FormAlert tone="warning">{applyError}</FormAlert>
        ) : null}

        <div className="flex flex-col gap-1.5">
          <label htmlFor="vendor-op-action" className="text-meta font-medium text-ink-1">
            {t('admin.vendors.ops_action')}
          </label>
          <select
            id="vendor-op-action"
            className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1"
            value={action}
            disabled={apply.isPending}
            onChange={(event) => {
              setAction(event.target.value as VendorOperationAction)
              setPreview(null)
              setFormError(undefined)
              setApplyError(undefined)
            }}
          >
            <option value="assign">{t('admin.vendors.ops_assign')}</option>
            <option value="merge">{t('admin.vendors.ops_merge')}</option>
            <option value="delete">{t('admin.vendors.ops_delete')}</option>
          </select>
          <p className="text-meta text-ink-3">
            {t(`admin.vendors.ops_${action}_hint`)}
          </p>
        </div>

        {action === 'assign' ? (
          <>
            <div className="flex flex-col gap-1.5">
              <label htmlFor="vendor-op-model-keyword" className="text-meta font-medium text-ink-1">
                {t('admin.vendors.ops_models')}
              </label>
              <input
                id="vendor-op-model-keyword"
                className="h-9 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1"
                placeholder={t('admin.vendors.ops_model_search')}
                value={modelKeyword}
                onChange={(event) => {
                  setModelKeyword(event.target.value)
                }}
              />
              <div className="max-h-48 overflow-y-auto rounded-lg border border-border-hairline p-2">
                {(modelsQuery.data?.items ?? []).map((model) => (
                  <label key={model.id} className="flex items-center gap-2 py-0.5 text-body">
                    <input
                      type="checkbox"
                      checked={modelIds.has(model.id)}
                      onChange={(event) => {
                        const checked = event.target.checked
                        setModelIds((current) => {
                          const next = new Set(current)
                          if (checked) next.add(model.id)
                          else next.delete(model.id)
                          return next
                        })
                        setPreview(null)
                      }}
                    />
                    <span className="font-mono text-ink-1">{model.model_name}</span>
                    {model.display_name !== null && model.display_name !== undefined ? (
                      <span className="text-meta text-ink-2">{model.display_name}</span>
                    ) : null}
                  </label>
                ))}
                {(modelsQuery.data?.items ?? []).length === 0 ? (
                  <p className="text-meta text-ink-3">{t('admin.vendors.ops_no_models')}</p>
                ) : null}
              </div>
            </div>
          </>
        ) : (
          <div className="flex flex-col gap-1.5">
            <span className="text-meta font-medium text-ink-1">
              {t('admin.vendors.ops_sources')}
            </span>
            <div className="max-h-40 overflow-y-auto rounded-lg border border-border-hairline p-2">
              {(vendorsQuery.data?.items ?? [])
                .filter((vendor) => vendorIds.has(vendor.id))
                .map((vendor) => (
                  <label key={vendor.id} className="flex items-center gap-2 py-0.5 text-body">
                    <input
                      type="checkbox"
                      checked
                      onChange={(event) => {
                        if (event.target.checked) return
                        setVendorIds((current) => {
                          const next = new Set(current)
                          next.delete(vendor.id)
                          return next
                        })
                        setPreview(null)
                      }}
                    />
                    <span className="text-ink-1">{vendor.name}</span>
                  </label>
                ))}
              {vendorsQuery.data === undefined || vendorIds.size === 0 ? (
                <p className="text-meta text-ink-3">{t('admin.vendors.ops_select_vendors')}</p>
              ) : null}
            </div>
          </div>
        )}

        <div className="flex flex-col gap-1.5">
          <label htmlFor="vendor-op-target" className="text-meta font-medium text-ink-1">
            {t('admin.vendors.ops_target')}
          </label>
          <select
            id="vendor-op-target"
            className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1"
            value={targetVendorId}
            disabled={action === 'delete'}
            onChange={(event) => {
              setTargetVendorId(event.target.value)
              setPreview(null)
            }}
          >
            <option value="">{t('admin.vendors.ops_target_none')}</option>
            {action === 'assign' ? (
              <option value="0">{t('admin.vendors.ops_target_clear')}</option>
            ) : null}
            {targetOptions.map((vendor) => (
              <option key={vendor.id} value={vendor.id}>
                {vendor.name}
              </option>
            ))}
          </select>
        </div>

        {preview === null ? (
          <div className="flex justify-end gap-2">
            <Button variant="secondary" onClick={() => onOpenChange(false)}>
              {t('common.action.cancel')}
            </Button>
            <Button disabled={runPreview.isPending} onClick={previewNow}>
              {t('admin.vendors.ops_preview')}
            </Button>
          </div>
        ) : (
          <section className="rounded-lg border border-border-hairline">
            <header className="flex items-center justify-between border-b border-border-hairline px-3 py-2">
              <h3 className="text-meta font-medium text-ink-1">
                {t('admin.vendors.ops_preview_title')}
              </h3>
              <StatusBadge variant={preview.conflict ? 'warning' : 'success'}>
                {preview.conflict
                  ? t('admin.vendors.ops_has_conflicts')
                  : t('admin.vendors.ops_no_conflicts')}
              </StatusBadge>
            </header>
            <div className="flex flex-col gap-2 px-3 py-2 text-body">
              <p className="text-ink-1">
                {t('admin.vendors.ops_affected', { count: preview.affected })}
              </p>
              {preview.action === 'assign' ? (
                <p className="text-meta text-ink-2">
                  {t('admin.vendors.ops_preview_assign', {
                    target:
                      targetVendorId === '0'
                        ? t('admin.vendors.ops_target_clear')
                        : vendorName(Number(targetVendorId)),
                  })}
                </p>
              ) : (
                <p className="text-meta text-ink-2">
                  {t('admin.vendors.ops_preview_vendors', {
                    names: [...vendorIds].map(vendorName).join(', '),
                  })}
                </p>
              )}
              <div className="text-meta text-ink-3">
                <span>{t('admin.vendors.ops_expected_version')}</span>{' '}
                <code className="font-mono text-ink-2">
                  {preview.expected_version.slice(0, 16)}…
                </code>
              </div>
              {preview.conflicts.length > 0 ? (
                <ul className="text-meta text-warning">
                  {preview.conflicts.map((conflict) => (
                    <li key={conflict.vendor_id}>
                      {vendorName(conflict.vendor_id)} · {conflict.current_version.slice(0, 12)}…
                    </li>
                  ))}
                </ul>
              ) : null}
            </div>
            <footer className="flex justify-end gap-2 border-t border-border-hairline px-3 py-2">
              <Button
                variant="secondary"
                onClick={() => {
                  setPreview(null)
                }}
              >
                {t('admin.vendors.ops_back')}
              </Button>
              <Button variant="secondary" disabled={runPreview.isPending} onClick={previewNow}>
                {t('admin.vendors.ops_repreview')}
              </Button>
              <Button
                disabled={
                  apply.isPending || preview.conflict || pending === null || pending.body === null
                }
                onClick={() => {
                  if (pending === null || pending.body === null) return
                  if (preview.expected_version === '') return
                  apply.mutate({
                    body: pending.body,
                    expectedVersion: preview.expected_version,
                  })
                }}
              >
                {t('admin.vendors.ops_apply')}
              </Button>
            </footer>
          </section>
        )}
      </div>
    </Dialog>
  )
}

function operationError(error: unknown, t: TranslateFn): string {
  if (error instanceof ApiError) {
    if (error.kind === 'conflict') return t('admin.vendors.ops_conflict')
    if (error.kind === 'forbidden') return t('admin.models.forbidden')
    if (error.kind === 'not_found') return t('admin.models.not_found')
    if (error.kind === 'validation') return t('admin.vendors.ops_invalid')
  }
  return t('admin.vendors.ops_error')
}
