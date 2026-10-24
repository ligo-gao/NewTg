import { useEffect, useState, type Dispatch, type FormEvent, type SetStateAction } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import type { components } from '@/api/schema'
import { requireData } from '@/api/result'
import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert, TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

type ModelMeta = components['schemas']['ModelMeta']
type ModelMetaCreateRequest = components['schemas']['ModelMetaCreateRequest']
type ModelMetaUpdateRequest = components['schemas']['ModelMetaUpdateRequest']
type ModelMetaNameRule = components['schemas']['ModelMetaNameRule']
type Capability = ModelMeta['capabilities'][number]
type Modality = ModelMeta['modalities'][number]
type Method = ModelMeta['endpoints'][string][number]
type ParameterType = ModelMeta['parameters'][number]['type']

const MODEL_NAME_PATTERN = /^[A-Za-z0-9._:/-]{1,128}$/
const DESCRIPTION_MAX = 512
// 与 backend/internal/store/model_meta_validate.go 的 maxModelMetaDisplayNameRunes
// 对齐：公开卡片标题行是 varchar(64)。
const DISPLAY_NAME_MAX = 64
const TAGS_MAX = 10
// 与 maxModelMetaTagRunes 对齐：单个标签 trim 后 1–32 字符。
const TAG_MAX_RUNES = 32
const ICON_MAX = 128
const KNOWLEDGE_CUTOFF_PATTERN = /^\d{4}-(0[1-9]|1[0-2])$/

/** `String.length` 数的是 UTF-16 码元；服务端按 rune 校验，前端必须同口径。 */
function runeLength(value: string): number {
  return Array.from(value).length
}

const CAPABILITIES: readonly Capability[] = [
  'text',
  'vision',
  'audio',
  'video',
  'function_calling',
  'tool_use',
  'reasoning',
  'embedding',
  'rerank',
  'image_generation',
  'speech',
  'transcription',
]
const MODALITIES: readonly Modality[] = ['text', 'image', 'audio', 'video']
const METHODS: readonly Method[] = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE']
const PARAMETER_TYPES: readonly ParameterType[] = [
  'string',
  'number',
  'integer',
  'boolean',
  'array',
  'object',
]
const NAME_RULES: readonly ModelMetaNameRule[] = [0, 1, 2, 3]

interface EndpointRow {
  path: string
  methods: Method[]
}

interface ParameterRow {
  name: string
  type: ParameterType
  required: boolean
  defaultValue: string
  description: string
}

export interface ModelMutateSheetProps {
  open: boolean
  mode: 'create' | 'edit'
  model?: ModelMeta
  /** 缺失模型抽屉「补建档」带过来的模型名（仅创建模式）。 */
  prefillModelName?: string
  onOpenChange: (open: boolean) => void
  onSaved: () => void
}

/**
 * F-06-22 新建/编辑抽屉（560px；1024–1279 为 −40px 的 520px）。
 *
 * 乐观锁：编辑回传列表行上的 `updated_at`；409 时保留操作者输入并刷新列表，
 * 重新打开抽屉即可从新版本继续（web-00 §5.6——不自动重试、不回滚输入）。
 *
 * `endpoints` 刻意做成「路径行 + 方法多选」的结构化编辑器：裸 JSON 输入
 * 既无法在保存前校验「键必须以 / 开头」，也容易把对象写成数组。
 */
export function ModelMutateSheet({
  open,
  mode,
  model,
  prefillModelName,
  onOpenChange,
  onSaved,
}: ModelMutateSheetProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const [modelName, setModelName] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [description, setDescription] = useState('')
  const [icon, setIcon] = useState('')
  const [vendorId, setVendorId] = useState('')
  const [tags, setTags] = useState<string[]>([])
  const [tagDraft, setTagDraft] = useState('')
  const [endpoints, setEndpoints] = useState<EndpointRow[]>([])
  const [capabilities, setCapabilities] = useState<Capability[]>([])
  const [contextLength, setContextLength] = useState('')
  const [maxOutput, setMaxOutput] = useState('')
  const [modalities, setModalities] = useState<Modality[]>([])
  const [knowledgeCutoff, setKnowledgeCutoff] = useState('')
  const [releasedAt, setReleasedAt] = useState('')
  const [parameters, setParameters] = useState<ParameterRow[]>([])
  const [status, setStatus] = useState<1 | 2>(1)
  const [syncOfficial, setSyncOfficial] = useState(true)
  const [nameRule, setNameRule] = useState<ModelMetaNameRule>(0)
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [conflict, setConflict] = useState(false)
  const [submitting, setSubmitting] = useState(false)

  // 供应商下拉：搜索端点最多返回 100 条（契约上限）；目录是百级规模，
  // 一次装载足够覆盖选择器，避免在抽屉里再造一套异步搜索状态。
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

  useEffect(() => {
    if (!open) return
    setModelName(model?.model_name ?? prefillModelName ?? '')
    setDisplayName(model?.display_name ?? '')
    setDescription(model?.description ?? '')
    setIcon(model?.icon ?? '')
    setVendorId(model?.vendor_id === null || model?.vendor_id === undefined ? '' : String(model.vendor_id))
    setTags(model?.tags ?? [])
    setTagDraft('')
    setEndpoints(
      Object.entries(model?.endpoints ?? {}).map(([path, methods]) => ({
        path,
        methods: [...methods],
      })),
    )
    setCapabilities([...(model?.capabilities ?? [])])
    setContextLength(model?.context_length === null || model?.context_length === undefined ? '' : String(model.context_length))
    setMaxOutput(model?.max_output === null || model?.max_output === undefined ? '' : String(model.max_output))
    setModalities([...(model?.modalities ?? [])])
    setKnowledgeCutoff(model?.knowledge_cutoff ?? '')
    setReleasedAt(model?.released_at ?? '')
    setParameters(
      (model?.parameters ?? []).map((parameter) => {
        const extra = parameter as Record<string, unknown>
        return {
          name: parameter.name,
          type: parameter.type,
          required: extra.required === true,
          defaultValue:
            typeof extra.default === 'string'
              ? extra.default
              : extra.default === undefined || extra.default === null
                ? ''
                : JSON.stringify(extra.default),
          description:
            typeof extra.description === 'string' ? extra.description : '',
        }
      }),
    )
    setStatus(model?.status ?? 1)
    setSyncOfficial(model?.sync_official ?? true)
    setNameRule(model?.name_rule ?? 0)
    setFieldErrors({})
    setFormError(undefined)
    setConflict(false)
    setSubmitting(false)
  }, [open, model, prefillModelName])

  function addTag(): void {
    const value = tagDraft.trim()
    if (value === '') return
    // 服务端 trim 后要求 1–32 rune；超长标签若放进选择集只会在保存时
    // 被整单拒绝，不如在这里就地拦下。
    if (runeLength(value) > TAG_MAX_RUNES) {
      setFieldErrors((current) => ({
        ...current,
        tags: t('admin.models.error_tag_length', { count: TAG_MAX_RUNES }),
      }))
      return
    }
    if (tags.length >= TAGS_MAX) {
      setFieldErrors((current) => ({
        ...current,
        tags: t('admin.models.tags_limit', { count: TAGS_MAX }),
      }))
      return
    }
    if (tags.includes(value)) {
      setTagDraft('')
      return
    }
    setTags((current) => [...current, value])
    setTagDraft('')
    setFieldErrors((current) => {
      const next = { ...current }
      delete next.tags
      return next
    })
  }

  function parseNullableInt(
    value: string,
    field: string,
    errors: Record<string, string>,
  ): number | null {
    const trimmed = value.trim()
    if (trimmed === '') return null
    const parsed = Number(trimmed)
    if (!Number.isSafeInteger(parsed) || parsed < 0) {
      errors[field] = t('admin.models.error_non_negative')
      return null
    }
    return parsed
  }

  async function save(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()

    const errors: Record<string, string> = {}
    if (mode === 'create' && !MODEL_NAME_PATTERN.test(modelName.trim())) {
      errors.model_name = t('admin.models.error_model_name')
    }
    if (runeLength(displayName) > DISPLAY_NAME_MAX) {
      errors.display_name = t('admin.models.error_display_name', {
        count: DISPLAY_NAME_MAX,
      })
    }
    if (description.length > DESCRIPTION_MAX) {
      errors.description = t('admin.models.error_description', { count: DESCRIPTION_MAX })
    }
    if (icon.length > ICON_MAX) {
      errors.icon = t('admin.models.error_icon', { count: ICON_MAX })
    }
    if (tags.length > TAGS_MAX) {
      errors.tags = t('admin.models.tags_limit', { count: TAGS_MAX })
    } else if (tags.some((tag) => runeLength(tag.trim()) > TAG_MAX_RUNES)) {
      errors.tags = t('admin.models.error_tag_length', { count: TAG_MAX_RUNES })
    }
    if (knowledgeCutoff.trim() !== '' && !KNOWLEDGE_CUTOFF_PATTERN.test(knowledgeCutoff.trim())) {
      errors.knowledge_cutoff = t('admin.models.error_knowledge_cutoff')
    }

    const endpointObject: Record<string, Method[]> = {}
    const seenPaths = new Set<string>()
    for (const row of endpoints) {
      const path = row.path.trim()
      if (path === '' && row.methods.length === 0) continue
      if (!path.startsWith('/')) {
        errors.endpoints = t('admin.models.error_endpoint_path')
        break
      }
      if (row.methods.length === 0) {
        errors.endpoints = t('admin.models.error_endpoint_methods')
        break
      }
      // 对象键会静默覆盖：重复 path 行若直接写进 endpointObject，后一行
      // 会悄悄吃掉前一行的方法集合。保存前必须报错。
      if (seenPaths.has(path)) {
        errors.endpoints = t('admin.models.error_endpoint_duplicate', { path })
        break
      }
      seenPaths.add(path)
      endpointObject[path] = row.methods
    }

    const parameterPayload: ({ name: string; type: ParameterType } & Record<string, unknown>)[] = []
    for (const row of parameters) {
      const name = row.name.trim()
      if (name === '') continue
      const entry: { name: string; type: ParameterType } & Record<string, unknown> = {
        name,
        type: row.type,
        required: row.required,
      }
      if (row.defaultValue.trim() !== '') entry.default = row.defaultValue.trim()
      if (row.description.trim() !== '') entry.description = row.description.trim()
      parameterPayload.push(entry)
    }

    const context = parseNullableInt(contextLength, 'context_length', errors)
    const output = parseNullableInt(maxOutput, 'max_output', errors)

    setFieldErrors(errors)
    setConflict(false)
    setFormError(undefined)
    if (Object.keys(errors).length > 0) return

    const common: Omit<ModelMetaUpdateRequest, 'updated_at'> = {
      display_name: displayName.trim() === '' ? null : displayName.trim(),
      description: description.trim() === '' ? null : description,
      icon: icon.trim() === '' ? null : icon.trim(),
      vendor_id: vendorId === '' ? null : Number(vendorId),
      tags,
      endpoints: endpointObject,
      capabilities,
      modalities,
      parameters: parameterPayload,
      context_length: context,
      max_output: output,
      knowledge_cutoff: knowledgeCutoff.trim() === '' ? null : knowledgeCutoff.trim(),
      released_at: releasedAt === '' ? null : releasedAt,
      status,
      sync_official: syncOfficial,
      name_rule: nameRule,
    }

    setSubmitting(true)
    try {
      if (mode === 'create') {
        const body: ModelMetaCreateRequest = {
          ...common,
          model_name: modelName.trim(),
        }
        await apiClient.POST('/api/models', { body })
        push({ tone: 'success', message: t('admin.models.create_success') })
      } else if (model !== undefined) {
        // 编辑时 model_name 只读：改名会切断渠道 supported_models 的隐式
        // 关联，也会让公开广场的 URL 漂移（F-06-22 冻结为不可改）。
        const body: ModelMetaUpdateRequest = { ...common, updated_at: model.updated_at }
        await apiClient.PUT('/api/models/{id}', {
          params: { path: { id: model.id } },
          body,
        })
        push({ tone: 'success', message: t('admin.models.update_success') })
      }
      await invalidateCatalog(queryClient)
      onSaved()
    } catch (error) {
      handleWriteError(error, {
        t,
        queryClient,
        setFieldErrors,
        setFormError,
        setConflict,
        mode,
      })
    } finally {
      setSubmitting(false)
    }
  }

  const vendorOptions = vendorsQuery.data?.items ?? []

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      variant="sheet"
      panelClassName="!max-w-[520px] xl:!max-w-[560px]"
      title={
        mode === 'create'
          ? t('admin.models.create_title')
          : t('admin.models.edit_title')
      }
    >
      <form className="flex flex-col gap-4" onSubmit={(event) => void save(event)} noValidate>
        {conflict ? <FormAlert tone="warning">{t('admin.models.conflict')}</FormAlert> : null}
        {formError !== undefined ? <FormAlert tone="danger">{formError}</FormAlert> : null}

        <TextField
          label={t('admin.models.field_model_name')}
          value={modelName}
          maxLength={128}
          readOnly={mode === 'edit'}
          disabled={mode === 'edit'}
          error={fieldErrors.model_name}
          onChange={(event) => {
            setModelName(event.target.value)
          }}
        />
        <TextField
          label={t('admin.models.field_display_name')}
          value={displayName}
          maxLength={DISPLAY_NAME_MAX}
          error={fieldErrors.display_name}
          onChange={(event) => {
            setDisplayName(event.target.value)
          }}
        />
        <div className="flex flex-col gap-1.5">
          <label htmlFor="model-field-description" className="text-meta font-medium text-ink-1">
            {t('admin.models.field_description')}
          </label>
          <textarea
            id="model-field-description"
            className="min-h-[88px] rounded-lg border border-border-default bg-surface-card px-3 py-2 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            maxLength={DESCRIPTION_MAX}
            aria-invalid={fieldErrors.description !== undefined ? true : undefined}
            value={description}
            onChange={(event) => {
              setDescription(event.target.value)
            }}
          />
          <p className="text-right text-meta text-ink-3">
            {t('admin.models.description_count', {
              count: description.length,
              max: DESCRIPTION_MAX,
            })}
          </p>
          {fieldErrors.description !== undefined ? (
            <p className="text-meta text-danger">{fieldErrors.description}</p>
          ) : null}
        </div>

        <TextField
          label={t('admin.models.field_icon')}
          value={icon}
          maxLength={ICON_MAX}
          hint={t('admin.models.field_icon_hint')}
          error={fieldErrors.icon}
          onChange={(event) => {
            setIcon(event.target.value)
          }}
        />

        <div className="flex flex-col gap-1.5">
          <label htmlFor="model-field-vendor" className="text-meta font-medium text-ink-1">
            {t('admin.models.field_vendor')}
          </label>
          <select
            id="model-field-vendor"
            className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1"
            value={vendorId}
            onChange={(event) => {
              setVendorId(event.target.value)
            }}
          >
            <option value="">{t('admin.models.vendor_none')}</option>
            {vendorOptions.map((vendor) => (
              <option key={vendor.id} value={vendor.id}>
                {vendor.name}
              </option>
            ))}
          </select>
          {fieldErrors.vendor_id !== undefined ? (
            <p className="text-meta text-danger">{fieldErrors.vendor_id}</p>
          ) : null}
        </div>

        <div className="flex flex-col gap-1.5">
          <span className="text-meta font-medium text-ink-1">{t('admin.models.field_tags')}</span>
          <div className="flex flex-wrap gap-1.5">
            {tags.map((tag) => (
              <span
                key={tag}
                className="inline-flex h-6 items-center gap-1 rounded-[6px] border border-neutral-line bg-neutral-bg px-2 text-badge text-neutral"
              >
                {tag}
                <button
                  type="button"
                  className="text-ink-3 hover:text-ink-1"
                  aria-label={t('admin.models.tag_remove', { tag })}
                  onClick={() => {
                    setTags((current) => current.filter((item) => item !== tag))
                  }}
                >
                  ×
                </button>
              </span>
            ))}
          </div>
          <div className="flex gap-2">
            <input
              aria-label={t('admin.models.field_tags')}
              className="h-10 flex-1 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1"
              maxLength={TAG_MAX_RUNES}
              value={tagDraft}
              onChange={(event) => {
                setTagDraft(event.target.value)
              }}
              onKeyDown={(event) => {
                if (event.key === 'Enter') {
                  event.preventDefault()
                  addTag()
                }
              }}
            />
            <Button type="button" variant="secondary" onClick={addTag}>
              {t('admin.models.tag_add')}
            </Button>
          </div>
          <p className="text-meta text-ink-3">{t('admin.models.tags_hint', { count: TAGS_MAX })}</p>
          {fieldErrors.tags !== undefined ? (
            <p className="text-meta text-danger">{fieldErrors.tags}</p>
          ) : null}
        </div>

        <fieldset className="rounded-lg border border-border-hairline p-3">
          <legend className="px-1 text-meta font-medium text-ink-1">
            {t('admin.models.field_endpoints')}
          </legend>
          <div className="flex flex-col gap-2">
            {endpoints.map((row, index) => (
              <div key={index} className="flex flex-col gap-1.5 border-b border-border-hairline pb-2 last:border-0">
                <div className="flex gap-2">
                  <input
                    aria-label={t('admin.models.endpoint_path')}
                    placeholder="/v1/chat/completions"
                    className="h-9 flex-1 rounded-lg border border-border-default bg-surface-card px-2 font-mono text-body text-ink-1"
                    value={row.path}
                    onChange={(event) => {
                      const path = event.target.value
                      setEndpoints((current) =>
                        current.map((item, itemIndex) =>
                          itemIndex === index ? { ...item, path } : item,
                        ),
                      )
                    }}
                  />
                  <Button
                    type="button"
                    variant="ghost"
                    className="h-9 px-2 text-meta text-danger hover:bg-danger-bg"
                    onClick={() => {
                      setEndpoints((current) => current.filter((_, itemIndex) => itemIndex !== index))
                    }}
                  >
                    {t('admin.models.endpoint_remove')}
                  </Button>
                </div>
                <div className="flex flex-wrap gap-3">
                  {METHODS.map((method) => (
                    <label key={method} className="flex items-center gap-1 text-meta text-ink-2">
                      <input
                        type="checkbox"
                        checked={row.methods.includes(method)}
                        onChange={(event) => {
                          const checked = event.target.checked
                          setEndpoints((current) =>
                            current.map((item, itemIndex) => {
                              if (itemIndex !== index) return item
                              const methods = checked
                                ? [...item.methods, method]
                                : item.methods.filter((value) => value !== method)
                              return { ...item, methods }
                            }),
                          )
                        }}
                      />
                      {method}
                    </label>
                  ))}
                </div>
              </div>
            ))}
            <div>
              <Button
                type="button"
                variant="secondary"
                className="h-8 px-2 text-meta"
                onClick={() => {
                  setEndpoints((current) => [...current, { path: '', methods: ['POST'] }])
                }}
              >
                {t('admin.models.endpoint_add')}
              </Button>
            </div>
            {fieldErrors.endpoints !== undefined ? (
              <p className="text-meta text-danger">{fieldErrors.endpoints}</p>
            ) : null}
          </div>
        </fieldset>

        <div className="grid grid-cols-2 gap-3">
          <div className="flex flex-col gap-1.5">
            <label htmlFor="model-field-status" className="text-meta font-medium text-ink-1">
              {t('admin.models.field_status')}
            </label>
            <select
              id="model-field-status"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1"
              value={status}
              onChange={(event) => {
                setStatus(Number(event.target.value) === 2 ? 2 : 1)
              }}
            >
              <option value={1}>{t('admin.models.status_enabled')}</option>
              <option value={2}>{t('admin.models.status_disabled')}</option>
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <label htmlFor="model-field-name-rule" className="text-meta font-medium text-ink-1">
              {t('admin.models.field_name_rule')}
            </label>
            <select
              id="model-field-name-rule"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1"
              value={nameRule}
              onChange={(event) => {
                const value = Number(event.target.value)
                setNameRule(
                  NAME_RULES.includes(value as ModelMetaNameRule)
                    ? (value as ModelMetaNameRule)
                    : 0,
                )
              }}
            >
              <option value={0}>{t('admin.models.name_rule_exact')}</option>
              <option value={1}>{t('admin.models.name_rule_prefix')}</option>
              <option value={2}>{t('admin.models.name_rule_contains')}</option>
              <option value={3}>{t('admin.models.name_rule_suffix')}</option>
            </select>
          </div>
        </div>

        <label className="flex items-start gap-2 text-body text-ink-1">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={syncOfficial}
            onChange={(event) => {
              setSyncOfficial(event.target.checked)
            }}
          />
          <span>
            {t('admin.models.field_sync_official')}
            <span className="block text-meta text-ink-3">
              {t('admin.models.field_sync_official_hint')}
            </span>
          </span>
        </label>

        <fieldset className="rounded-lg border border-border-hairline p-3">
          <legend className="px-1 text-meta font-medium text-ink-1">
            {t('admin.models.field_capabilities')}
          </legend>
          <div className="grid grid-cols-2 gap-1">
            {CAPABILITIES.map((capability) => (
              <label key={capability} className="flex items-center gap-1.5 text-meta text-ink-2">
                <input
                  type="checkbox"
                  checked={capabilities.includes(capability)}
                  onChange={(event) => {
                    const checked = event.target.checked
                    setCapabilities((current) =>
                      checked
                        ? [...current, capability]
                        : current.filter((value) => value !== capability),
                    )
                  }}
                />
                {t(`admin.models.capability_${capability}`)}
              </label>
            ))}
          </div>
        </fieldset>

        <div className="grid grid-cols-2 gap-3">
          <TextField
            label={t('admin.models.field_context_length')}
            inputMode="numeric"
            value={contextLength}
            error={fieldErrors.context_length}
            onChange={(event) => {
              setContextLength(event.target.value)
            }}
          />
          <TextField
            label={t('admin.models.field_max_output')}
            inputMode="numeric"
            value={maxOutput}
            error={fieldErrors.max_output}
            onChange={(event) => {
              setMaxOutput(event.target.value)
            }}
          />
        </div>

        <fieldset className="rounded-lg border border-border-hairline p-3">
          <legend className="px-1 text-meta font-medium text-ink-1">
            {t('admin.models.field_modalities')}
          </legend>
          <div className="flex flex-wrap gap-4">
            {MODALITIES.map((modality) => (
              <label key={modality} className="flex items-center gap-1.5 text-meta text-ink-2">
                <input
                  type="checkbox"
                  checked={modalities.includes(modality)}
                  onChange={(event) => {
                    const checked = event.target.checked
                    setModalities((current) =>
                      checked
                        ? [...current, modality]
                        : current.filter((value) => value !== modality),
                    )
                  }}
                />
                {t(`admin.models.modality_${modality}`)}
              </label>
            ))}
          </div>
        </fieldset>

        <div className="grid grid-cols-2 gap-3">
          <TextField
            label={t('admin.models.field_knowledge_cutoff')}
            type="month"
            value={knowledgeCutoff}
            error={fieldErrors.knowledge_cutoff}
            onChange={(event) => {
              setKnowledgeCutoff(event.target.value)
            }}
          />
          <TextField
            label={t('admin.models.field_released_at')}
            type="date"
            value={releasedAt}
            onChange={(event) => {
              setReleasedAt(event.target.value)
            }}
          />
        </div>

        <fieldset className="rounded-lg border border-border-hairline p-3">
          <legend className="px-1 text-meta font-medium text-ink-1">
            {t('admin.models.field_parameters')}
          </legend>
          <div className="flex flex-col gap-2">
            {parameters.map((row, index) => (
              <div key={index} className="grid grid-cols-[1fr_100px] gap-2 border-b border-border-hairline pb-2 last:border-0">
                <input
                  aria-label={t('admin.models.param_name')}
                  placeholder={t('admin.models.param_name')}
                  className="h-9 rounded-lg border border-border-default bg-surface-card px-2 font-mono text-body text-ink-1"
                  value={row.name}
                  onChange={(event) => {
                    const name = event.target.value
                    setParameters((current) =>
                      current.map((item, itemIndex) =>
                        itemIndex === index ? { ...item, name } : item,
                      ),
                    )
                  }}
                />
                <select
                  aria-label={t('admin.models.param_type')}
                  className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1"
                  value={row.type}
                  onChange={(event) => {
                    const type = event.target.value as ParameterType
                    setParameters((current) =>
                      current.map((item, itemIndex) =>
                        itemIndex === index ? { ...item, type } : item,
                      ),
                    )
                  }}
                >
                  {PARAMETER_TYPES.map((type) => (
                    <option key={type} value={type}>
                      {type}
                    </option>
                  ))}
                </select>
                <input
                  aria-label={t('admin.models.param_default')}
                  placeholder={t('admin.models.param_default')}
                  className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1"
                  value={row.defaultValue}
                  onChange={(event) => {
                    const defaultValue = event.target.value
                    setParameters((current) =>
                      current.map((item, itemIndex) =>
                        itemIndex === index ? { ...item, defaultValue } : item,
                      ),
                    )
                  }}
                />
                <input
                  aria-label={t('admin.models.param_description')}
                  placeholder={t('admin.models.param_description')}
                  className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1"
                  value={row.description}
                  onChange={(event) => {
                    const description = event.target.value
                    setParameters((current) =>
                      current.map((item, itemIndex) =>
                        itemIndex === index ? { ...item, description } : item,
                      ),
                    )
                  }}
                />
                <div className="col-span-2 flex items-center justify-between">
                  <label className="flex items-center gap-1.5 text-meta text-ink-2">
                    <input
                      type="checkbox"
                      checked={row.required}
                      onChange={(event) => {
                        const required = event.target.checked
                        setParameters((current) =>
                          current.map((item, itemIndex) =>
                            itemIndex === index ? { ...item, required } : item,
                          ),
                        )
                      }}
                    />
                    {t('admin.models.param_required')}
                  </label>
                  <Button
                    type="button"
                    variant="ghost"
                    className="h-8 px-2 text-meta text-danger hover:bg-danger-bg"
                    onClick={() => {
                      setParameters((current) => current.filter((_, itemIndex) => itemIndex !== index))
                    }}
                  >
                    {t('admin.models.param_remove')}
                  </Button>
                </div>
              </div>
            ))}
            <div>
              <Button
                type="button"
                variant="secondary"
                className="h-8 px-2 text-meta"
                onClick={() => {
                  setParameters((current) => [
                    ...current,
                    { name: '', type: 'string', required: false, defaultValue: '', description: '' },
                  ])
                }}
              >
                {t('admin.models.param_add')}
              </Button>
            </div>
          </div>
        </fieldset>

        <div className="flex justify-end gap-2">
          <Button
            type="button"
            variant="secondary"
            onClick={() => {
              onOpenChange(false)
            }}
          >
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

interface WriteErrorOptions {
  t: TranslateFn
  queryClient: ReturnType<typeof useQueryClient>
  setFieldErrors: Dispatch<SetStateAction<Record<string, string>>>
  setFormError: (value: string | undefined) => void
  setConflict: (value: boolean) => void
  /** 创建与编辑对 409 的语义不同：创建没有乐观锁版本。 */
  mode: 'create' | 'edit'
}

/**
 * 抽屉里真正渲染了行内错误的字段。`problem.fields` 可能包含这些集合之外的
 * 列（capabilities / parameters / status…），只写进 state 不渲染会让 422
 * 变成「点了保存什么都没发生」，必须汇总到表单级提示。
 */
const RENDERED_MODEL_FIELDS = new Set<string>([
  'model_name',
  'display_name',
  'description',
  'icon',
  'vendor_id',
  'tags',
  'endpoints',
  'context_length',
  'max_output',
  'knowledge_cutoff',
])

/**
 * 目录写操作的字段级错误映射。服务端把列名放在 Problem 的 `fields`
 * 扩展里（例如 `vendor_id`）；前端只在已知字段上渲染行内错误，未知字段
 * 统一落到表单级提示，避免把列名直接暴露给用户。
 */
export function handleWriteError(
  error: unknown,
  { t, queryClient, setFieldErrors, setFormError, setConflict, mode }: WriteErrorOptions,
): void {
  if (!(error instanceof ApiError)) {
    setFormError(t('admin.models.action_error'))
    return
  }
  if (error.kind === 'conflict') {
    if (mode === 'create') {
      // 创建路径没有 `updated_at` 乐观锁，409 只可能是重名（唯一索引）。
      // 复用编辑态的「记录已被他人修改」文案会把管理员引向错误的排查方向。
      setFieldErrors((current) => ({
        ...current,
        model_name: t('admin.models.error_model_name_exists'),
      }))
      return
    }
    setConflict(true)
    // 409 表示行已被别处修改：界面上的 updated_at 已过期，必须重查，
    // 否则再次保存会反复撞同一个版本（web-00 §5.6）。
    void queryClient.invalidateQueries({ queryKey: ['admin-models'] })
    void queryClient.invalidateQueries({ queryKey: ['admin-vendors'] })
    return
  }
  if (error.kind === 'validation') {
    const fields = error.fields ?? []
    const errors: Record<string, string> = {}
    for (const field of fields) {
      errors[field] = fieldMessage(field, t)
    }
    if (Object.keys(errors).length > 0) {
      setFieldErrors(errors)
      const hidden = fields.filter((field) => !RENDERED_MODEL_FIELDS.has(field))
      if (hidden.length > 0) {
        setFormError(
          t('admin.models.error_fields_summary', { fields: hidden.join(', ') }),
        )
      }
      return
    }
    setFormError(error.detail ?? t('admin.models.action_error'))
    return
  }
  if (error.kind === 'forbidden') {
    setFormError(t('admin.models.forbidden'))
    return
  }
  setFormError(error.detail ?? t('admin.models.action_error'))
}

function fieldMessage(field: string, t: TranslateFn): string {
  switch (field) {
    case 'model_name':
      return t('admin.models.error_model_name')
    case 'display_name':
      // 带占位符的文案必须在这里补参数；只返回词典键会渲染出字面量 {count}。
      return t('admin.models.error_display_name', { count: DISPLAY_NAME_MAX })
    case 'vendor_id':
      return t('admin.models.error_vendor')
    case 'endpoints':
      return t('admin.models.error_endpoints')
    case 'tags':
      return t('admin.models.error_tags')
    case 'description':
      return t('admin.models.error_description_generic')
    case 'knowledge_cutoff':
      return t('admin.models.error_knowledge_cutoff')
    case 'context_length':
    case 'max_output':
      return t('admin.models.error_non_negative')
    default:
      return t('admin.models.error_field', { field })
  }
}

/** 写操作后目录、缺失清单与供应商下拉都可能变化，统一失效。 */
export async function invalidateCatalog(
  queryClient: ReturnType<typeof useQueryClient>,
): Promise<void> {
  await Promise.all([
    queryClient.invalidateQueries({ queryKey: ['admin-models'] }),
    queryClient.invalidateQueries({ queryKey: ['admin-vendors'] }),
    queryClient.invalidateQueries({ queryKey: ['admin-models-missing'] }),
    queryClient.invalidateQueries({ queryKey: ['admin-vendor-options'] }),
  ])
}
