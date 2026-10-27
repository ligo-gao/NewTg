import { useEffect, useState, type ChangeEvent, type FormEvent } from 'react'
import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { filenameFromContentDisposition } from '@/features/logs/export-api'
import { csrfFetch } from '@/lib/csrf'
import { downloadBlob } from '@/shared/lib/download'
import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { FormAlert, TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

type BulkRowError = components['schemas']['BulkRowError']
type CustomerType = NonNullable<components['schemas']['BulkGenerateRequest']['customer_type']>

export interface BulkCreateDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  onCreated: () => void
}

type Tab = 'import' | 'generate'

const MAX_COUNT = 500
const TEMPLATE_FALLBACK = 'newtg-users-template.csv'

/**
 * 服务端错误码 → 文案。未知码回退到通用行错误，绝不把机器码直接打给管理员。
 * (Codes are frozen in the contract; see `BulkRowError.code`.)
 */
function rowErrorMessage(t: TranslateFn, code: string): string {
  const key = `admin.bulk.errors_${code}`
  const message = t(key)
  return message === key ? t('admin.bulk.errors_unknown') : message
}

/**
 * Reads the per-row `errors[]` array from a failed response. The bulk import
 * endpoint emits it for both 400 (header/no-data-row) and 422 (row validation);
 * treating only 422 as "fix these rows" made header errors look retryable.
 * Returns undefined when the body is not JSON or carries no errors.
 */
async function readRowErrors(response: Response): Promise<BulkRowError[] | undefined> {
  try {
    const body = (await response.json()) as { errors?: unknown }
    const errors = body.errors
    if (Array.isArray(errors) && errors.length > 0) {
      return errors as BulkRowError[]
    }
  } catch {
    // Non-JSON error body: fall through to the generic message.
  }
  return undefined
}

/**
 * Reads the RFC 7807 `fields` array from a 422 problem body. The generate
 * endpoint answers "that group does not exist" with `fields:["group"]`;
 * collapsing that into the generic retry message would ask the operator to
 * retry an input that can never succeed.
 */
async function readValidationFields(response: Response): Promise<string[] | undefined> {
  if (response.status !== 422) return undefined
  try {
    const body = (await response.json()) as { fields?: unknown }
    const fields = body.fields
    if (Array.isArray(fields) && fields.length > 0) {
      return fields.filter((field): field is string => typeof field === 'string')
    }
  } catch {
    // Non-JSON error body: fall through to the generic message.
  }
  return undefined
}

function fallbackGenerateFilename(): string {
  const now = new Date()
  const pad = (value: number): string => String(value).padStart(2, '0')
  return (
    'newtg-users-' +
    `${now.getUTCFullYear()}${pad(now.getUTCMonth() + 1)}${pad(now.getUTCDate())}` +
    `T${pad(now.getUTCHours())}${pad(now.getUTCMinutes())}${pad(now.getUTCSeconds())}Z.csv`
  )
}

/**
 * 批量建号弹层（两条路径）：
 * - CSV 导入：`multipart/form-data`，口令由管理员在文件里自带，服务端只回
 *   `{created}`；422 是**逐行**错误数组，按行号列出且不关闭弹层；
 * - 系统生成：服务端返回 `text/csv` 明文口令，前端只下载一次，UI 明示
 *   「关闭后无法再次查看」且不提供重新下载。
 *
 * 两个端点都不在 `apiClient` 的能力范围内（multipart / 二进制），因此走
 * `csrfFetch`——它与契约客户端共用 CSRF 头与 403 重放，不另造一套。
 */
export function BulkCreateDialog({ open, onOpenChange, onCreated }: BulkCreateDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)

  const [tab, setTab] = useState<Tab>('import')

  // Import tab.
  const [file, setFile] = useState<File | null>(null)
  const [mustChangePassword, setMustChangePassword] = useState(true)
  const [importing, setImporting] = useState(false)
  const [importError, setImportError] = useState<string | undefined>(undefined)
  const [rowErrors, setRowErrors] = useState<BulkRowError[]>([])
  const [templateDownloading, setTemplateDownloading] = useState(false)
  const [templateError, setTemplateError] = useState<string | undefined>(undefined)

  // Generate tab.
  const [count, setCount] = useState('')
  const [customerType, setCustomerType] = useState<'' | CustomerType>('')
  const [group, setGroup] = useState('')
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [generating, setGenerating] = useState(false)
  const [generateError, setGenerateError] = useState<string | undefined>(undefined)
  const [generatedCount, setGeneratedCount] = useState<number | null>(null)
  // The "save the file now" notice is literally false once the save failed, so
  // the two must never be on screen together.
  const [downloadFailed, setDownloadFailed] = useState(false)

  useEffect(() => {
    if (!open) return
    setTab('import')
    setFile(null)
    setMustChangePassword(true)
    setImporting(false)
    setImportError(undefined)
    setRowErrors([])
    setTemplateDownloading(false)
    setTemplateError(undefined)
    setCount('')
    setCustomerType('')
    setGroup('')
    setFieldErrors({})
    setGenerating(false)
    setGenerateError(undefined)
    setGeneratedCount(null)
    setDownloadFailed(false)
  }, [open])

  // Same cache entry the user list's filter uses, so both selectors agree.
  const groupsQuery = useQuery({
    queryKey: ['admin-groups', 'filter-options'],
    enabled: open && tab === 'generate',
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/groups', {
          params: { query: { p: 1, page_size: 100 } },
        }),
      ),
  })

  function handleFileChange(event: ChangeEvent<HTMLInputElement>): void {
    setFile(event.target.files?.[0] ?? null)
    setImportError(undefined)
    setRowErrors([])
  }

  async function downloadTemplate(): Promise<void> {
    setTemplateError(undefined)
    setTemplateDownloading(true)
    try {
      const response = await csrfFetch('/api/users/bulk/template', {
        method: 'GET',
        headers: { Accept: 'text/csv' },
      })
      if (!response.ok) {
        setTemplateError(t('admin.bulk.template_error'))
        return
      }
      const filename =
        filenameFromContentDisposition(response.headers.get('Content-Disposition')) ??
        TEMPLATE_FALLBACK
      downloadBlob(await response.blob(), filename)
    } catch {
      setTemplateError(t('admin.bulk.template_error'))
    } finally {
      setTemplateDownloading(false)
    }
  }

  async function submitImport(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    if (file === null) {
      setImportError(t('admin.bulk.file_required'))
      return
    }
    setImportError(undefined)
    setRowErrors([])
    setImporting(true)
    const form = new FormData()
    form.append('file', file)
    form.append('must_change_password', mustChangePassword ? 'true' : 'false')
    try {
      const response = await csrfFetch('/api/users/bulk/import', {
        method: 'POST',
        body: form,
        headers: { Accept: 'application/json' },
      })
      if (response.ok) {
        const result = (await response.json()) as { created?: number }
        const created = result.created ?? 0
        push({
          tone: 'success',
          message: t('admin.bulk.imported', { count: created }),
        })
        onCreated()
        onOpenChange(false)
        return
      }
      // Header/row validation arrives as `{errors:[…]}` on 400 *and* 422
      // (handlers_bulk_users.go maps a malformed header and a wrong field count
      // to 400). Sniff the body instead of trusting one status: collapsing
      // these into "try again" would ask the operator to retry a file that can
      // never succeed. Keep the dialog open so the file can be fixed.
      const rowErrorsFromBody = await readRowErrors(response)
      if (rowErrorsFromBody !== undefined) {
        setRowErrors(rowErrorsFromBody)
        return
      }
      // A body over the 1 MiB cap has no row errors to list, but it is still a
      // deterministic input error with its own message.
      setImportError(
        response.status === 413
          ? t('admin.bulk.errors_file_too_large')
          : t('admin.bulk.import_error'),
      )
    } catch {
      setImportError(t('admin.bulk.import_error'))
    } finally {
      setImporting(false)
    }
  }

  async function submitGenerate(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    const parsed = Number(count)
    // Trim once: the server trims too, so a whitespace-only name must fail
    // here as an empty field instead of becoming a 422 the UI cannot place.
    const trimmedGroup = group.trim()
    const errors: Record<string, string> = {}
    if (!Number.isInteger(parsed) || parsed < 1 || parsed > MAX_COUNT) {
      errors.count = t('admin.bulk.count_invalid')
    }
    if (customerType === '') errors.customerType = t('admin.bulk.customer_type_required')
    if (trimmedGroup === '') errors.group = t('admin.bulk.group_required')
    setFieldErrors(errors)
    if (Object.keys(errors).length > 0) return

    setGenerateError(undefined)
    setGenerating(true)
    try {
      const response = await csrfFetch('/api/users/bulk/generate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Accept: 'text/csv' },
        body: JSON.stringify({
          count: parsed,
          customer_type: customerType,
          group: trimmedGroup,
        }),
      })
      if (!response.ok) {
        // A typed group the server does not know is a field-level error, not a
        // transient failure: say so on the field and keep the dialog open.
        const fields = await readValidationFields(response)
        if (fields !== undefined) {
          const fieldLevel: Record<string, string> = {}
          if (fields.includes('group')) {
            fieldLevel.group = t('admin.bulk.errors_group_not_found')
          }
          if (fields.includes('count')) {
            fieldLevel.count = t('admin.bulk.count_invalid')
          }
          if (fields.includes('customer_type')) {
            fieldLevel.customerType = t('admin.bulk.customer_type_required')
          }
          if (Object.keys(fieldLevel).length > 0) {
            setFieldErrors(fieldLevel)
            return
          }
        }
        setGenerateError(t('admin.bulk.generate_error'))
        return
      }
      const filename =
        filenameFromContentDisposition(response.headers.get('Content-Disposition')) ??
        fallbackGenerateFilename()
      // The accounts exist on the server now and the CSV is the only copy of
      // their passwords. Record success before downloading: if the save throws,
      // putting the form back would invite a retry that creates a second batch
      // while the first batch's passwords are gone for good.
      setGeneratedCount(parsed)
      onCreated()
      try {
        downloadBlob(await response.blob(), filename)
        push({
          tone: 'success',
          message: t('admin.bulk.generated', { count: parsed }),
        })
      } catch {
        // No success toast on this path: it says "save the file now", which is
        // exactly what just failed. The dedicated alert is the whole message.
        setDownloadFailed(true)
        setGenerateError(t('admin.bulk.generated_download_failed'))
      }
    } catch {
      setGenerateError(t('admin.bulk.generate_error'))
    } finally {
      setGenerating(false)
    }
  }

  const generateDisabled =
    generating || count.trim() === '' || customerType === '' || group.trim() === ''

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('admin.bulk.title')}
      footer={
        <Button variant="secondary" onClick={() => onOpenChange(false)}>
          {t('common.action.close')}
        </Button>
      }
    >
      <div
        role="tablist"
        aria-label={t('admin.bulk.tabs_label')}
        className="flex flex-wrap gap-1 border-b border-border-hairline"
      >
        {(['import', 'generate'] as const).map((entry) => {
          const selected = entry === tab
          return (
            <button
              key={entry}
              id={`bulk-tab-${entry}`}
              type="button"
              role="tab"
              aria-selected={selected}
              tabIndex={selected ? 0 : -1}
              onClick={() => {
                setTab(entry)
              }}
              className={[
                'h-10 rounded-t-lg px-3 text-meta',
                selected
                  ? 'border-b-2 border-brand font-medium text-ink-1'
                  : 'text-ink-2 hover:bg-surface-hover hover:text-ink-1',
              ].join(' ')}
            >
              {entry === 'import' ? t('admin.bulk.tab_import') : t('admin.bulk.tab_generate')}
            </button>
          )
        })}
      </div>

      {tab === 'import' ? (
        <div
          role="tabpanel"
          id="bulk-panel-import"
          aria-labelledby="bulk-tab-import"
          className="flex flex-col gap-3 pt-3"
        >
          <form
            id="bulk-import-form"
            className="flex flex-col gap-3"
            onSubmit={(event) => void submitImport(event)}
            noValidate
          >
            {rowErrors.length > 0 ? (
              <FormAlert>
                <p>{t('admin.bulk.errors_title')}</p>
                <ul className="mt-2 list-disc space-y-1 pl-5">
                  {rowErrors.map((error) => (
                    <li key={`${error.line}-${error.field}-${error.code}`}>
                      <span>{t('admin.bulk.error_line', { line: error.line })}</span>
                      {error.username !== undefined && error.username !== '' ? (
                        <span>{` · @${error.username}`}</span>
                      ) : null}
                      <span>{` — ${rowErrorMessage(t, error.code)}`}</span>
                    </li>
                  ))}
                </ul>
              </FormAlert>
            ) : null}
            {importError !== undefined ? <FormAlert>{importError}</FormAlert> : null}
            {templateError !== undefined ? <FormAlert>{templateError}</FormAlert> : null}

            <div className="flex flex-col gap-1.5">
              <label htmlFor="bulk-import-file" className="text-body font-medium text-ink-1">
                {t('admin.bulk.file')}
              </label>
              <input
                id="bulk-import-file"
                type="file"
                accept=".csv,text/csv"
                className="h-10 w-full rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 file:mr-3 file:rounded-md file:border-0 file:bg-surface-subtle file:px-2 file:py-1 file:text-meta file:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                onChange={handleFileChange}
              />
              <p className="text-meta text-ink-3">{t('admin.bulk.file_hint')}</p>
            </div>

            <label className="flex items-center gap-2 text-body text-ink-1">
              <input
                type="checkbox"
                checked={mustChangePassword}
                onChange={(event) => {
                  setMustChangePassword(event.target.checked)
                }}
              />
              {t('admin.bulk.must_change_password')}
            </label>

            <div className="flex justify-end gap-2">
              <Button
                type="button"
                variant="secondary"
                disabled={templateDownloading}
                onClick={() => void downloadTemplate()}
              >
                {t('admin.bulk.template_download')}
              </Button>
              <Button type="submit" disabled={file === null || importing}>
                {importing ? t('admin.bulk.importing') : t('admin.bulk.import_submit')}
              </Button>
            </div>
          </form>
        </div>
      ) : (
        <div
          role="tabpanel"
          id="bulk-panel-generate"
          aria-labelledby="bulk-tab-generate"
          className="flex flex-col gap-3 pt-3"
        >
          {downloadFailed ? null : (
            <FormAlert tone="info">{t('admin.bulk.credentials_notice')}</FormAlert>
          )}
          {generatedCount !== null ? (
            <>
              {/* The success line ends with "save the file now"; on a failed
                  download the failure alert below is the only truthful text. */}
              {downloadFailed ? null : (
                <FormAlert tone="success">
                  {t('admin.bulk.generated', { count: generatedCount })}
                </FormAlert>
              )}
              {generateError !== undefined ? <FormAlert>{generateError}</FormAlert> : null}
            </>
          ) : (
            <form
              id="bulk-generate-form"
              className="flex flex-col gap-3"
              onSubmit={(event) => void submitGenerate(event)}
              noValidate
            >
              {generateError !== undefined ? <FormAlert>{generateError}</FormAlert> : null}
              <TextField
                label={t('admin.bulk.count')}
                type="number"
                min={1}
                max={MAX_COUNT}
                value={count}
                onChange={(event) => {
                  setCount(event.target.value)
                }}
                error={fieldErrors.count}
                hint={t('admin.bulk.count_hint')}
              />
              <div className="flex flex-col gap-1.5">
                <label
                  htmlFor="bulk-customer-type"
                  className="text-meta font-medium text-ink-1"
                >
                  {t('admin.bulk.customer_type')}
                </label>
                <select
                  id="bulk-customer-type"
                  className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                  value={customerType}
                  aria-invalid={fieldErrors.customerType !== undefined ? true : undefined}
                  aria-describedby={
                    fieldErrors.customerType !== undefined
                      ? 'bulk-customer-type-error'
                      : undefined
                  }
                  onChange={(event) => {
                    setCustomerType(event.target.value as '' | CustomerType)
                    setFieldErrors((current) => {
                      const next = { ...current }
                      delete next.customerType
                      return next
                    })
                  }}
                >
                  <option value="">{t('admin.bulk.choose')}</option>
                  <option value="general">
                    {t('admin.users.customer_type_general')}
                  </option>
                  <option value="enterprise">
                    {t('admin.users.customer_type_enterprise')}
                  </option>
                </select>
                {fieldErrors.customerType !== undefined ? (
                  <p id="bulk-customer-type-error" className="text-meta text-danger">
                    {fieldErrors.customerType}
                  </p>
                ) : null}
              </div>
              <div className="flex flex-col gap-1.5">
                <label htmlFor="bulk-group" className="text-meta font-medium text-ink-1">
                  {t('admin.bulk.group')}
                </label>
                <input
                  id="bulk-group"
                  type="text"
                  list="bulk-group-suggestions"
                  autoComplete="off"
                  className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                  value={group}
                  aria-invalid={fieldErrors.group !== undefined ? true : undefined}
                  aria-describedby={
                    fieldErrors.group !== undefined
                      ? 'bulk-group-error'
                      : groupsQuery.isError
                        ? 'bulk-group-load-failed'
                        : 'bulk-group-hint'
                  }
                  onChange={(event) => {
                    setGroup(event.target.value)
                    setFieldErrors((current) => {
                      const next = { ...current }
                      delete next.group
                      return next
                    })
                  }}
                />
                {/* Suggestions are optional: GET /api/groups needs groups:manage,
                    while bulk generation only needs users:create. A typed group
                    name is validated by the server (store.ExistingGroups). */}
                <datalist id="bulk-group-suggestions">
                  {(groupsQuery.data?.items ?? []).map((candidate) => (
                    <option key={candidate.name} value={candidate.name} />
                  ))}
                </datalist>
                {fieldErrors.group === undefined && groupsQuery.isError ? (
                  <p id="bulk-group-load-failed" className="text-meta text-ink-3">
                    {t('admin.bulk.group_load_failed')}
                  </p>
                ) : null}
                {fieldErrors.group === undefined && !groupsQuery.isError ? (
                  <p id="bulk-group-hint" className="text-meta text-ink-3">
                    {t('admin.bulk.group_hint')}
                  </p>
                ) : null}
                {fieldErrors.group !== undefined ? (
                  <p id="bulk-group-error" className="text-meta text-danger">
                    {fieldErrors.group}
                  </p>
                ) : null}
              </div>
              <div className="flex justify-end">
                <Button type="submit" disabled={generateDisabled}>
                  {generating ? t('admin.bulk.generating') : t('admin.bulk.generate_submit')}
                </Button>
              </div>
            </form>
          )}
        </div>
      )}
    </Dialog>
  )
}
