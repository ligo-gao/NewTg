import { useEffect, useState, type FormEvent } from 'react'
import { useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import type { components } from '@/api/schema'
import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert, TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

type Vendor = components['schemas']['Vendor']
type VendorCreateRequest = components['schemas']['VendorCreateRequest']
type VendorUpdateRequest = components['schemas']['VendorUpdateRequest']

const DESCRIPTION_MAX = 512
const ICON_MAX = 128

export interface VendorMutateSheetProps {
  open: boolean
  mode: 'create' | 'edit'
  vendor?: Vendor
  onOpenChange: (open: boolean) => void
  onSaved: () => void
}

/**
 * F-06-23 供应商新建/编辑抽屉。
 *
 * 乐观锁不是 `updated_at`：服务端返回的 `version` 是 7 列规范化的派生
 * sha256，更新必须原样回传；界面把它作为只读字段展示，避免管理员误以为
 * 可以手改。
 */
export function VendorMutateSheet({
  open,
  mode,
  vendor,
  onOpenChange,
  onSaved,
}: VendorMutateSheetProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [icon, setIcon] = useState('')
  const [status, setStatus] = useState<1 | 2>(1)
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [conflict, setConflict] = useState(false)
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    if (!open) return
    setName(vendor?.name ?? '')
    setDescription(vendor?.description ?? '')
    setIcon(vendor?.icon ?? '')
    setStatus(vendor?.status ?? 1)
    setFieldErrors({})
    setFormError(undefined)
    setConflict(false)
    setSubmitting(false)
  }, [open, vendor])

  async function save(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    const errors: Record<string, string> = {}
    if (name.trim() === '') errors.name = t('admin.vendors.error_name')
    if (description.length > DESCRIPTION_MAX) {
      errors.description = t('admin.models.error_description', { count: DESCRIPTION_MAX })
    }
    if (icon.length > ICON_MAX) {
      errors.icon = t('admin.models.error_icon', { count: ICON_MAX })
    }
    setFieldErrors(errors)
    setConflict(false)
    setFormError(undefined)
    if (Object.keys(errors).length > 0) return

    setSubmitting(true)
    try {
      if (mode === 'create') {
        const body: VendorCreateRequest = {
          name: name.trim(),
          description: description.trim() === '' ? null : description,
          icon: icon.trim() === '' ? null : icon.trim(),
          status,
        }
        await apiClient.POST('/api/vendors', { body })
        push({ tone: 'success', message: t('admin.vendors.create_success') })
      } else if (vendor !== undefined) {
        const body: VendorUpdateRequest = {
          name: name.trim(),
          description: description.trim() === '' ? null : description,
          icon: icon.trim() === '' ? null : icon.trim(),
          status,
          version: vendor.version,
        }
        await apiClient.PUT('/api/vendors/{id}', {
          params: { path: { id: vendor.id } },
          body,
        })
        push({ tone: 'success', message: t('admin.vendors.update_success') })
      }
      await queryClient.invalidateQueries({ queryKey: ['admin-vendors'] })
      await queryClient.invalidateQueries({ queryKey: ['admin-vendor-options'] })
      onSaved()
    } catch (error) {
      if (error instanceof ApiError && error.kind === 'conflict') {
        // 重名（大小写不敏感）与 version 不符都映射为 409。列表必须重查，
        // 否则再次保存会带着同一个过期 version 重试。
        setConflict(true)
        void queryClient.invalidateQueries({ queryKey: ['admin-vendors'] })
      } else if (error instanceof ApiError && error.kind === 'validation') {
        const messages: Record<string, string> = {}
        for (const field of error.fields ?? []) {
          if (field === 'name' || field === 'description' || field === 'icon') {
            messages[field] = t('admin.vendors.error_field', { field })
          }
        }
        if (Object.keys(messages).length > 0) setFieldErrors(messages)
        else setFormError(error.detail ?? t('admin.vendors.action_error'))
      } else {
        setFormError(vendorActionError(error, t))
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      variant="sheet"
      panelClassName="!max-w-[520px] xl:!max-w-[560px]"
      title={
        mode === 'create'
          ? t('admin.vendors.create_title')
          : t('admin.vendors.edit_title')
      }
    >
      <form className="flex flex-col gap-4" onSubmit={(event) => void save(event)} noValidate>
        {conflict ? <FormAlert tone="warning">{t('admin.vendors.conflict')}</FormAlert> : null}
        {formError !== undefined ? <FormAlert tone="danger">{formError}</FormAlert> : null}

        <TextField
          label={t('admin.vendors.field_name')}
          value={name}
          maxLength={128}
          error={fieldErrors.name}
          onChange={(event) => {
            setName(event.target.value)
          }}
        />

        <div className="flex flex-col gap-1.5">
          <label htmlFor="vendor-field-description" className="text-[13px] font-medium text-ink-1">
            {t('admin.vendors.field_description')}
          </label>
          <textarea
            id="vendor-field-description"
            className="min-h-[88px] rounded-lg border border-border-default bg-surface-card px-3 py-2 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            maxLength={DESCRIPTION_MAX}
            value={description}
            onChange={(event) => {
              setDescription(event.target.value)
            }}
          />
          <p className="text-right text-xs text-ink-3">
            {t('admin.models.description_count', {
              count: description.length,
              max: DESCRIPTION_MAX,
            })}
          </p>
          {fieldErrors.description !== undefined ? (
            <p className="text-xs text-danger">{fieldErrors.description}</p>
          ) : null}
        </div>

        <TextField
          label={t('admin.vendors.field_icon')}
          value={icon}
          maxLength={ICON_MAX}
          error={fieldErrors.icon}
          onChange={(event) => {
            setIcon(event.target.value)
          }}
        />

        <div className="flex flex-col gap-1.5">
          <label htmlFor="vendor-field-status" className="text-[13px] font-medium text-ink-1">
            {t('admin.vendors.field_status')}
          </label>
          <select
            id="vendor-field-status"
            className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1"
            value={status}
            onChange={(event) => {
              setStatus(Number(event.target.value) === 2 ? 2 : 1)
            }}
          >
            <option value={1}>{t('admin.models.status_enabled')}</option>
            <option value={2}>{t('admin.models.status_disabled')}</option>
          </select>
        </div>

        {mode === 'edit' && vendor !== undefined ? (
          <div className="flex flex-col gap-1 text-xs text-ink-3">
            <span>{t('admin.vendors.field_version')}</span>
            <code className="break-all font-mono text-ink-2">{vendor.version}</code>
            <span>{t('admin.vendors.field_version_hint')}</span>
          </div>
        ) : null}

        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={() => onOpenChange(false)}>
            {t('common.action.cancel')}
          </Button>
          <Button type="submit" disabled={submitting}>
            {t('admin.models.save')}
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

export function vendorActionError(error: unknown, t: TranslateFn): string {
  if (error instanceof ApiError) {
    if (error.kind === 'conflict') return t('admin.vendors.conflict')
    if (error.kind === 'not_found') return t('admin.models.not_found')
    if (error.kind === 'validation') return t('admin.models.validation_error')
    if (error.kind === 'forbidden') return t('admin.models.forbidden')
  }
  return t('admin.vendors.action_error')
}
