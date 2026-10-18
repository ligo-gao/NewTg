import { useQuery } from '@tanstack/react-query'
import { useId, useState, type FormEvent, type ReactNode } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { pushErrorToast, useToastStore } from '@/shared/ui/toast'

import {
  createChannel,
  fieldErrorsOf,
  getChannel,
  updateChannel,
} from './api'
import { ChannelTypeSelect } from './channel-type-select'
import { Sheet } from './ui/sheet'
import {
  REQUIRED_BASE_URL_TYPES,
  isChannelType,
  type Channel,
  type ChannelCredentials,
  type CreateChannelInput,
  type UpdateChannelInput,
} from './types'

export interface ChannelMutateSheetProps {
  open: boolean
  mode: 'create' | 'edit'
  /** Required in edit mode; the sheet loads the detail for the mask + version. */
  channelId?: number
  onOpenChange: (open: boolean) => void
  onSaved: () => void
}

/**
 * Create/edit sheet (modules-06 §3, F-06-03…F-06-05).
 *
 * Credential handling is write-only: the detail response only ever carries a
 * mask (`credentials_masked`) and `credentials_set`, so the form never receives
 * plaintext. On edit the API key input starts empty and is only serialised when
 * the administrator types a new key — leaving it blank keeps the stored secret.
 */
export function ChannelMutateSheet({
  open,
  mode,
  channelId,
  onOpenChange,
  onSaved,
}: ChannelMutateSheetProps) {
  const { t } = useI18n()
  const editing = mode === 'edit'
  const detailQuery = useQuery({
    queryKey: ['admin', 'channels', 'detail', channelId],
    queryFn: () => getChannel(channelId as number),
    enabled: open && editing && channelId !== undefined,
  })

  if (!open) return null

  const title = editing
    ? t('common.channels.form.editTitle', {
        name: detailQuery.data?.name ?? '…',
      })
    : t('common.channels.form.createTitle')

  let body: ReactNode
  if (!editing) {
    body = (
      <ChannelForm
        key="create"
        mode="create"
        onCancel={() => {
          onOpenChange(false)
        }}
        onSaved={onSaved}
      />
    )
  } else if (detailQuery.isPending) {
    body = (
      <div data-testid="channel-form-skeleton" className="flex flex-col gap-3">
        <div className="h-9 w-2/3 animate-pulse rounded-lg bg-surface-subtle" />
        <div className="h-9 w-1/2 animate-pulse rounded-lg bg-surface-subtle" />
      </div>
    )
  } else if (detailQuery.isError || detailQuery.data === undefined) {
    body = (
      <div role="alert" className="flex flex-col gap-2 text-sm text-danger">
        <p>{t('common.channels.error.load')}</p>
        <Button variant="secondary" onClick={() => void detailQuery.refetch()}>
          {t('common.channels.error.retry')}
        </Button>
      </div>
    )
  } else {
    body = (
      <ChannelForm
        key={detailQuery.data.id}
        mode="edit"
        channel={detailQuery.data}
        onCancel={() => {
          onOpenChange(false)
        }}
        onSaved={onSaved}
      />
    )
  }

  return (
    <Sheet open={open} onOpenChange={onOpenChange} title={title}>
      {body}
    </Sheet>
  )
}

// ---------------------------------------------------------------------------
// Form
// ---------------------------------------------------------------------------

interface ChannelFormState {
  name: string
  type: string
  baseUrl: string
  apiKey: string
  deployment: string
  apiVersion: string
  models: string[]
  allGroups: boolean
  groups: string
  priority: string
  weight: string
  rateLimit: string
  autoBan: boolean
  testModel: string
  remark: string
  tag: string
}

type FieldKey =
  | 'name'
  | 'type'
  | 'baseUrl'
  | 'apiKey'
  | 'deployment'
  | 'apiVersion'
  | 'models'
  | 'groups'
  | 'priority'
  | 'weight'
  | 'rateLimit'
  | 'testModel'
  | 'remark'
  | 'tag'

type FieldErrors = Partial<Record<FieldKey, string>>

function initialForm(channel?: Channel): ChannelFormState {
  if (channel === undefined) {
    return {
      name: '',
      type: 'openai',
      baseUrl: '',
      apiKey: '',
      deployment: '',
      apiVersion: '',
      models: [],
      allGroups: true,
      groups: '',
      priority: '0',
      weight: '1',
      rateLimit: '',
      autoBan: true,
      testModel: '',
      remark: '',
      tag: '',
    }
  }
  return {
    name: channel.name,
    type: channel.type,
    // Detail responses deliberately omit base_url and credentials (GC-6), so
    // both are write-only fields. Deployment/api_version are non-secret and do
    // come back in the mask.
    baseUrl: '',
    apiKey: '',
    deployment: channel.credentials_masked?.deployment ?? '',
    apiVersion: channel.credentials_masked?.api_version ?? '',
    models: [...channel.supported_models],
    allGroups: channel.group_tags === null,
    groups: (channel.group_tags ?? []).join(', '),
    priority: String(channel.priority),
    weight: String(channel.weight),
    rateLimit: channel.rate_limit === null ? '' : String(channel.rate_limit),
    autoBan: channel.auto_ban,
    testModel: channel.test_model ?? '',
    remark: channel.remark ?? '',
    tag: channel.tag ?? '',
  }
}

function parseGroups(raw: string): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const part of raw.split(',')) {
    const value = part.trim()
    if (value !== '' && !seen.has(value)) {
      seen.add(value)
      out.push(value)
    }
  }
  return out
}

function parseInteger(raw: string): number | null {
  if (raw.trim() === '') return null
  const value = Number(raw)
  return Number.isInteger(value) ? value : Number.NaN
}

function isValidHttpUrl(raw: string): boolean {
  try {
    const url = new URL(raw)
    return (
      (url.protocol === 'http:' || url.protocol === 'https:') &&
      url.hostname !== '' &&
      url.username === '' &&
      url.password === ''
    )
  } catch {
    return false
  }
}

const SERVER_FIELD_MAP: Record<string, FieldKey> = {
  name: 'name',
  type: 'type',
  base_url: 'baseUrl',
  'credentials.api_key': 'apiKey',
  'credentials.deployment': 'deployment',
  'credentials.api_version': 'apiVersion',
  supported_models: 'models',
  group_tags: 'groups',
  priority: 'priority',
  weight: 'weight',
  rate_limit: 'rateLimit',
  test_model: 'testModel',
  remark: 'remark',
  tag: 'tag',
}

function isConflictType(error: ApiError, slug: string): boolean {
  return error.type === slug || error.type.endsWith(`/${slug}`) || error.type.endsWith(`:${slug}`)
}

interface FieldProps {
  id: string
  label: string
  error?: string
  hint?: string
  children: ReactNode
}

function Field({ id, label, error, hint, children }: FieldProps) {
  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={id} className="text-[13px] text-ink-2">
        {label}
      </label>
      {children}
      {hint === undefined ? null : (
        <p className="text-xs text-ink-3">{hint}</p>
      )}
      {error === undefined ? null : (
        <p id={`${id}-error`} className="text-xs text-danger">
          {error}
        </p>
      )}
    </div>
  )
}

const INPUT_CLASSES =
  'h-9 w-full rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

interface ChannelFormProps {
  mode: 'create' | 'edit'
  channel?: Channel
  onCancel: () => void
  onSaved: () => void
}

function ChannelForm({ mode, channel, onCancel, onSaved }: ChannelFormProps) {
  const { t } = useI18n()
  const formId = useId()
  const [form, setForm] = useState<ChannelFormState>(() => initialForm(channel))
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>({})
  const [formError, setFormError] = useState<string | null>(null)
  const [credentialNotice, setCredentialNotice] = useState<string | null>(null)
  const [modelInput, setModelInput] = useState('')
  const [showApiKey, setShowApiKey] = useState(false)
  const [submitting, setSubmitting] = useState(false)

  const originalType = channel?.type
  const idFor = (field: FieldKey): string => `${formId}-${field}`
  const errorFor = (field: FieldKey): string | undefined => fieldErrors[field]

  function patch<K extends keyof ChannelFormState>(
    key: K,
    value: ChannelFormState[K],
  ): void {
    setForm((current) => ({ ...current, [key]: value }))
  }

  function handleTypeChange(nextType: string): void {
    if (!isChannelType(nextType)) return
    // modules-06 §3.2: switching type resets credential fields but keeps
    // name/base_url. The notice is announced politely for screen readers.
    setForm((current) => ({
      ...current,
      type: nextType,
      apiKey: '',
      deployment: '',
      apiVersion: '',
    }))
    setCredentialNotice(t('common.channels.form.credentialsReset'))
    setFieldErrors((current) => {
      const next = { ...current }
      delete next.apiKey
      delete next.deployment
      delete next.apiVersion
      return next
    })
  }

  function addModels(raw: string): void {
    const additions = raw
      .split(',')
      .map((part) => part.trim())
      .filter((part) => part !== '')
    if (additions.length === 0) return
    setForm((current) => {
      const seen = new Set(current.models)
      const models = [...current.models]
      for (const model of additions) {
        if (!seen.has(model)) {
          seen.add(model)
          models.push(model)
        }
      }
      return { ...current, models }
    })
  }

  function removeModel(model: string): void {
    setForm((current) => ({
      ...current,
      models: current.models.filter((entry) => entry !== model),
      testModel: current.testModel === model ? '' : current.testModel,
    }))
  }

  function validate(): FieldErrors {
    const errors: FieldErrors = {}
    const name = form.name.trim()
    if (name === '') {
      errors.name = t('common.channels.form.required')
    } else if ([...name].length > 64) {
      errors.name = t('common.channels.form.nameTooLong')
    }

    if (!isChannelType(form.type)) {
      errors.type = t('common.channels.form.required')
    }

    const baseUrl = form.baseUrl.trim()
    const typeChanged = mode === 'edit' && form.type !== originalType
    const baseUrlRequired =
      REQUIRED_BASE_URL_TYPES.has(form.type) && (mode === 'create' || typeChanged)
    if (baseUrl !== '') {
      if (!isValidHttpUrl(baseUrl)) errors.baseUrl = t('common.channels.form.invalidUrl')
    } else if (baseUrlRequired) {
      errors.baseUrl = t('common.channels.form.baseUrlRequired')
    }

    const apiKey = form.apiKey.trim()
    if (mode === 'create' && apiKey === '') {
      errors.apiKey = t('common.channels.form.apiKeyRequired')
    } else if (apiKey !== '' && apiKey.length < 8) {
      errors.apiKey = t('common.channels.form.apiKeyMin')
    }

    if (form.type === 'azure_openai' && (mode === 'create' || apiKey !== '')) {
      if (form.deployment.trim() === '') {
        errors.deployment = t('common.channels.form.deploymentRequired')
      }
      const apiVersion = form.apiVersion.trim()
      if (apiVersion === '') {
        errors.apiVersion = t('common.channels.form.apiVersionRequired')
      } else if (!/^\d{4}-\d{2}-\d{2}$/.test(apiVersion)) {
        errors.apiVersion = t('common.channels.form.apiVersionFormat')
      }
    }

    if (form.models.length === 0) {
      errors.models = t('common.channels.form.modelsRequired')
    }

    if (!form.allGroups && parseGroups(form.groups).length === 0) {
      errors.groups = t('common.channels.form.groupsRequired')
    }

    const priority = parseInteger(form.priority)
    if (priority === null || Number.isNaN(priority) || priority < -1000 || priority > 1000) {
      errors.priority = t('common.channels.form.priorityRange')
    }
    const weight = parseInteger(form.weight)
    if (weight === null || Number.isNaN(weight) || weight < 0 || weight > 1000) {
      errors.weight = t('common.channels.form.weightRange')
    }
    const rateLimit = parseInteger(form.rateLimit)
    if (
      form.rateLimit.trim() !== '' &&
      (rateLimit === null || Number.isNaN(rateLimit) || rateLimit < 1 || rateLimit > 100000)
    ) {
      errors.rateLimit = t('common.channels.form.rateLimitRange')
    }

    const testModel = form.testModel.trim()
    if (testModel !== '' && !form.models.includes(testModel)) {
      errors.testModel = t('common.channels.form.testModelMember')
    }
    if ([...form.remark].length > 255) {
      errors.remark = t('common.channels.form.remarkTooLong')
    }
    if ([...form.tag.trim()].length > 32) {
      errors.tag = t('common.channels.form.tagTooLong')
    }
    return errors
  }

  function focusFirstError(errors: FieldErrors): void {
    const first = (Object.keys(errors) as FieldKey[])[0]
    if (first === undefined) return
    setTimeout(() => {
      document.getElementById(idFor(first))?.focus()
    }, 0)
  }

  function buildCreateInput(): CreateChannelInput {
    const credentials: ChannelCredentials = { api_key: form.apiKey.trim() }
    if (form.type === 'azure_openai') {
      credentials.deployment = form.deployment.trim()
      credentials.api_version = form.apiVersion.trim()
    }
    const input: CreateChannelInput = {
      name: form.name.trim(),
      type: form.type,
      credentials,
      supported_models: form.models,
      group_tags: form.allGroups ? null : parseGroups(form.groups),
      priority: parseInteger(form.priority) ?? 0,
      weight: parseInteger(form.weight) ?? 1,
      rate_limit: parseInteger(form.rateLimit),
      auto_ban: form.autoBan,
      test_model: form.testModel.trim() === '' ? null : form.testModel.trim(),
      remark: form.remark.trim() === '' ? null : form.remark.trim(),
      tag: form.tag.trim() === '' ? null : form.tag.trim(),
    }
    if (form.baseUrl.trim() !== '') input.base_url = form.baseUrl.trim()
    return input
  }

  function buildUpdateInput(target: Channel): UpdateChannelInput {
    const input: UpdateChannelInput = {
      expected_version: target.version,
      name: form.name.trim(),
      type: form.type,
      supported_models: form.models,
      group_tags: form.allGroups ? null : parseGroups(form.groups),
      priority: parseInteger(form.priority) ?? 0,
      weight: parseInteger(form.weight) ?? 1,
      rate_limit: parseInteger(form.rateLimit),
      auto_ban: form.autoBan,
      test_model: form.testModel.trim() === '' ? null : form.testModel.trim(),
      remark: form.remark.trim() === '' ? null : form.remark.trim(),
      tag: form.tag.trim() === '' ? null : form.tag.trim(),
    }
    if (form.baseUrl.trim() !== '') input.base_url = form.baseUrl.trim()
    // F-06-03: a blank key means "keep the stored credential"; the field must
    // not appear in the PATCH body at all (not even as null/empty).
    if (form.apiKey.trim() !== '') {
      const credentials: ChannelCredentials = { api_key: form.apiKey.trim() }
      if (form.type === 'azure_openai') {
        if (form.deployment.trim() !== '') {
          credentials.deployment = form.deployment.trim()
        }
        if (form.apiVersion.trim() !== '') {
          credentials.api_version = form.apiVersion.trim()
        }
      }
      input.credentials = credentials
    }
    return input
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    if (submitting) return
    const errors = validate()
    setFieldErrors(errors)
    setFormError(null)
    if (Object.keys(errors).length > 0) {
      focusFirstError(errors)
      return
    }

    setSubmitting(true)
    try {
      if (mode === 'create') {
        await createChannel(buildCreateInput())
        useToastStore.getState().push({
          tone: 'success',
          message: t('common.channels.toast.created'),
        })
      } else if (channel !== undefined) {
        await updateChannel(channel.id, buildUpdateInput(channel))
        useToastStore.getState().push({
          tone: 'success',
          message: t('common.channels.toast.saved'),
        })
      }
      onSaved()
    } catch (error) {
      if (error instanceof ApiError) {
        if (isConflictType(error, 'channel_name_conflict')) {
          setFieldErrors({ name: t('common.channels.errors.nameExists') })
          focusFirstError({ name: 'name' })
        } else if (isConflictType(error, 'channel_version_conflict')) {
          setFormError(error.detail ?? t('common.channels.errors.conflict'))
        } else {
          const serverFields = fieldErrorsOf(error)
          if (serverFields.length > 0) {
            const mapped: FieldErrors = {}
            for (const entry of serverFields) {
              const key = SERVER_FIELD_MAP[entry.field]
              if (key !== undefined && mapped[key] === undefined) {
                mapped[key] = entry.message
              }
            }
            setFieldErrors(mapped)
          }
          setFormError(error.detail ?? error.title)
          pushErrorToast(error)
        }
      } else {
        setFormError(t('common.channels.errors.generic'))
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <form
      id={formId}
      onSubmit={(event) => {
        void handleSubmit(event)
      }}
      noValidate
      className="flex flex-col gap-8 pb-2"
    >
      {formError === null ? null : (
        <p role="alert" className="rounded-lg bg-danger-bg px-3 py-2 text-sm text-danger">
          {formError}
        </p>
      )}

      <section className="flex flex-col gap-4">
        <h3 className="text-[15px] font-semibold text-ink-1">
          {t('common.channels.form.basics')}
        </h3>

        <Field id={idFor('name')} label={t('common.channels.form.name')} error={errorFor('name')}>
          <input
            id={idFor('name')}
            value={form.name}
            onChange={(event) => {
              patch('name', event.target.value)
            }}
            disabled={submitting}
            aria-invalid={errorFor('name') !== undefined}
            aria-describedby={errorFor('name') === undefined ? undefined : `${idFor('name')}-error`}
            className={INPUT_CLASSES}
          />
        </Field>

        <Field id={idFor('type')} label={t('common.channels.form.type')} error={errorFor('type')}>
          <ChannelTypeSelect
            id={idFor('type')}
            value={form.type}
            onChange={handleTypeChange}
            disabled={submitting}
            aria-invalid={errorFor('type') !== undefined}
            aria-describedby={errorFor('type') === undefined ? undefined : `${idFor('type')}-error`}
          />
        </Field>
        {credentialNotice === null ? null : (
          <p aria-live="polite" className="text-xs text-ink-3">
            {credentialNotice}
          </p>
        )}

        <Field
          id={idFor('baseUrl')}
          label={t('common.channels.form.baseUrl')}
          error={errorFor('baseUrl')}
          hint={mode === 'edit' ? t('common.channels.form.baseUrlKeep') : undefined}
        >
          <input
            id={idFor('baseUrl')}
            type="url"
            value={form.baseUrl}
            placeholder={t('common.channels.form.baseUrlPlaceholder')}
            onChange={(event) => {
              patch('baseUrl', event.target.value)
            }}
            disabled={submitting}
            aria-invalid={errorFor('baseUrl') !== undefined}
            aria-describedby={errorFor('baseUrl') === undefined ? undefined : `${idFor('baseUrl')}-error`}
            className={INPUT_CLASSES}
          />
        </Field>

        <Field id={idFor('apiKey')} label={t('common.channels.form.apiKey')} error={errorFor('apiKey')}>
          <div className="flex items-center gap-2">
            <input
              id={idFor('apiKey')}
              type={showApiKey ? 'text' : 'password'}
              value={form.apiKey}
              placeholder={
                mode === 'edit'
                  ? t('common.channels.form.apiKeyKeep', {
                      mask: channel?.credentials_masked?.api_key ?? '',
                    })
                  : t('common.channels.form.apiKeyPlaceholder')
              }
              autoComplete="new-password"
              onChange={(event) => {
                patch('apiKey', event.target.value)
              }}
              disabled={submitting}
              aria-invalid={errorFor('apiKey') !== undefined}
              aria-describedby={errorFor('apiKey') === undefined ? undefined : `${idFor('apiKey')}-error`}
              className={INPUT_CLASSES}
            />
            <Button
              variant="secondary"
              aria-pressed={showApiKey}
              aria-label={
                showApiKey
                  ? t('common.channels.form.hideKey')
                  : t('common.channels.form.showKey')
              }
              disabled={submitting}
              onClick={() => {
                setShowApiKey((current) => !current)
              }}
            >
              {showApiKey ? (
                <svg
                  aria-hidden="true"
                  viewBox="0 0 20 20"
                  className="size-4"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.5"
                >
                  <path d="M2 10s3-5 8-5 8 5 8 5-3 5-8 5-8-5-8-5Z" />
                  <circle cx="10" cy="10" r="2.5" />
                </svg>
              ) : (
                <svg
                  aria-hidden="true"
                  viewBox="0 0 20 20"
                  className="size-4"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.5"
                >
                  <path d="M3 3l14 14M8.5 5.2A9.7 9.7 0 0 1 10 5c5 0 8 5 8 5a13.7 13.7 0 0 1-3 3.4M5.2 6.7A13.6 13.6 0 0 0 2 10s3 5 8 5a9.7 9.7 0 0 0 3.1-.5" />
                </svg>
              )}
            </Button>
          </div>
        </Field>

        {form.type === 'azure_openai' ? (
          <div className="grid grid-cols-2 gap-4">
            <Field
              id={idFor('deployment')}
              label={t('common.channels.form.deployment')}
              error={errorFor('deployment')}
            >
              <input
                id={idFor('deployment')}
                value={form.deployment}
                onChange={(event) => {
                  patch('deployment', event.target.value)
                }}
                disabled={submitting}
                aria-invalid={errorFor('deployment') !== undefined}
                aria-describedby={
                  errorFor('deployment') === undefined
                    ? undefined
                    : `${idFor('deployment')}-error`
                }
                className={INPUT_CLASSES}
              />
            </Field>
            <Field
              id={idFor('apiVersion')}
              label={t('common.channels.form.apiVersion')}
              error={errorFor('apiVersion')}
            >
              <input
                id={idFor('apiVersion')}
                value={form.apiVersion}
                placeholder="2024-10-21"
                onChange={(event) => {
                  patch('apiVersion', event.target.value)
                }}
                disabled={submitting}
                aria-invalid={errorFor('apiVersion') !== undefined}
                aria-describedby={
                  errorFor('apiVersion') === undefined
                    ? undefined
                    : `${idFor('apiVersion')}-error`
                }
                className={INPUT_CLASSES}
              />
            </Field>
          </div>
        ) : null}
      </section>

      <section className="flex flex-col gap-4">
        <h3 className="text-[15px] font-semibold text-ink-1">
          {t('common.channels.form.modelsSection')}
        </h3>

        <Field id={idFor('models')} label={t('common.channels.form.models')} error={errorFor('models')}>
          <div className="flex flex-col gap-2">
            <div className="flex flex-wrap gap-1.5">
              {form.models.map((model) => (
                <span
                  key={model}
                  className="inline-flex items-center gap-1 rounded-md border border-border-default bg-surface-subtle px-2 py-0.5 text-xs text-ink-1"
                >
                  {model}
                  <button
                    type="button"
                    aria-label={t('common.channels.form.removeModel', { model })}
                    className="text-ink-3 hover:text-ink-1"
                    onClick={() => {
                      removeModel(model)
                    }}
                    disabled={submitting}
                  >
                    ×
                  </button>
                </span>
              ))}
            </div>
            <input
              id={idFor('models')}
              value={modelInput}
              placeholder={t('common.channels.form.modelsPlaceholder')}
              onChange={(event) => {
                setModelInput(event.target.value)
              }}
              onKeyDown={(event) => {
                if (event.key === 'Enter' || event.key === ',') {
                  event.preventDefault()
                  addModels(modelInput)
                  setModelInput('')
                }
              }}
              onBlur={() => {
                if (modelInput.trim() !== '') {
                  addModels(modelInput)
                  setModelInput('')
                }
              }}
              disabled={submitting}
              aria-invalid={errorFor('models') !== undefined}
              aria-describedby={errorFor('models') === undefined ? undefined : `${idFor('models')}-error`}
              className={INPUT_CLASSES}
            />
          </div>
        </Field>

        <label className="flex items-center gap-2 text-sm text-ink-1">
          <input
            type="checkbox"
            checked={form.allGroups}
            onChange={(event) => {
              patch('allGroups', event.target.checked)
            }}
            disabled={submitting}
          />
          {t('common.channels.form.allGroups')}
        </label>

        {form.allGroups ? null : (
          <Field
            id={idFor('groups')}
            label={t('common.channels.form.groups')}
            error={errorFor('groups')}
          >
            <input
              id={idFor('groups')}
              value={form.groups}
              placeholder={t('common.channels.form.groupsPlaceholder')}
              onChange={(event) => {
                patch('groups', event.target.value)
              }}
              disabled={submitting}
              aria-invalid={errorFor('groups') !== undefined}
              aria-describedby={errorFor('groups') === undefined ? undefined : `${idFor('groups')}-error`}
              className={INPUT_CLASSES}
            />
          </Field>
        )}
      </section>

      <section className="flex flex-col gap-4">
        <h3 className="text-[15px] font-semibold text-ink-1">
          {t('common.channels.form.routing')}
        </h3>

        <div className="grid grid-cols-3 gap-4">
          <Field
            id={idFor('priority')}
            label={t('common.channels.form.priority')}
            error={errorFor('priority')}
            hint={t('common.channels.form.priorityHint')}
          >
            <input
              id={idFor('priority')}
              type="number"
              value={form.priority}
              onChange={(event) => {
                patch('priority', event.target.value)
              }}
              disabled={submitting}
              aria-invalid={errorFor('priority') !== undefined}
              aria-describedby={errorFor('priority') === undefined ? undefined : `${idFor('priority')}-error`}
              className={INPUT_CLASSES}
            />
          </Field>
          <Field
            id={idFor('weight')}
            label={t('common.channels.form.weight')}
            error={errorFor('weight')}
            hint={t('common.channels.form.weightHint')}
          >
            <input
              id={idFor('weight')}
              type="number"
              value={form.weight}
              onChange={(event) => {
                patch('weight', event.target.value)
              }}
              disabled={submitting}
              aria-invalid={errorFor('weight') !== undefined}
              aria-describedby={errorFor('weight') === undefined ? undefined : `${idFor('weight')}-error`}
              className={INPUT_CLASSES}
            />
          </Field>
          <Field
            id={idFor('rateLimit')}
            label={t('common.channels.form.rateLimit')}
            error={errorFor('rateLimit')}
          >
            <input
              id={idFor('rateLimit')}
              type="number"
              value={form.rateLimit}
              onChange={(event) => {
                patch('rateLimit', event.target.value)
              }}
              disabled={submitting}
              aria-invalid={errorFor('rateLimit') !== undefined}
              aria-describedby={errorFor('rateLimit') === undefined ? undefined : `${idFor('rateLimit')}-error`}
              className={INPUT_CLASSES}
            />
          </Field>
        </div>

        <label className="flex items-center gap-2 text-sm text-ink-1">
          <input
            type="checkbox"
            checked={form.autoBan}
            onChange={(event) => {
              patch('autoBan', event.target.checked)
            }}
            disabled={submitting}
          />
          {t('common.channels.form.autoBan')}
        </label>

        <Field
          id={idFor('testModel')}
          label={t('common.channels.form.testModel')}
          error={errorFor('testModel')}
        >
          <select
            id={idFor('testModel')}
            value={form.testModel}
            onChange={(event) => {
              patch('testModel', event.target.value)
            }}
            disabled={submitting}
            aria-invalid={errorFor('testModel') !== undefined}
            aria-describedby={errorFor('testModel') === undefined ? undefined : `${idFor('testModel')}-error`}
            className={INPUT_CLASSES}
          >
            <option value="">{t('common.channels.form.none')}</option>
            {form.models.map((model) => (
              <option key={model} value={model}>
                {model}
              </option>
            ))}
          </select>
        </Field>

        <Field id={idFor('remark')} label={t('common.channels.form.remark')} error={errorFor('remark')}>
          <textarea
            id={idFor('remark')}
            value={form.remark}
            onChange={(event) => {
              patch('remark', event.target.value)
            }}
            disabled={submitting}
            aria-invalid={errorFor('remark') !== undefined}
            aria-describedby={errorFor('remark') === undefined ? undefined : `${idFor('remark')}-error`}
            className="min-h-20 w-full rounded-lg border border-border-default bg-surface-card px-3 py-2 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
          />
        </Field>

        <Field id={idFor('tag')} label={t('common.channels.form.tag')} error={errorFor('tag')}>
          <input
            id={idFor('tag')}
            value={form.tag}
            onChange={(event) => {
              patch('tag', event.target.value)
            }}
            disabled={submitting}
            aria-invalid={errorFor('tag') !== undefined}
            aria-describedby={errorFor('tag') === undefined ? undefined : `${idFor('tag')}-error`}
            className={INPUT_CLASSES}
          />
        </Field>
      </section>

      <div className="flex justify-end gap-2 border-t border-border-hairline pt-4">
        <Button variant="secondary" onClick={onCancel}>
          {t('common.channels.form.cancel')}
        </Button>
        <Button type="submit" disabled={submitting}>
          {submitting ? t('common.channels.form.saving') : t('common.channels.form.save')}
        </Button>
      </div>
    </form>
  )
}
