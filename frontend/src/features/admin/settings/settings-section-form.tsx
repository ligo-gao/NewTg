import { useEffect, useId, useMemo, useRef, useState, type ChangeEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { ApiError } from '@/api/errors'
import { useCountdown } from '@/shared/lib/use-countdown'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert, TextField } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'
import { useToastStore } from '@/shared/ui/toast'

import {
  createIdempotencyKey,
  fetchSettingsSection,
  fieldErrorsOf,
  saveSettingsSection,
  testWebhook,
  type SettingsItem,
} from './api'
import type { FieldConfig, SectionConfig } from './sections'

interface SettingsSectionFormProps {
  config: SectionConfig
  /** Missing `settings:manage` keeps the card visible but read-only. */
  readOnly: boolean
  onDirtyChange: (sectionId: string, dirty: boolean) => void
}

type Draft = Record<string, unknown>
type FieldErrors = Record<string, string>

/** Human-readable message for a normalized transport error. */
function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : String(error)
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function normalizeForCompare(value: unknown): unknown {
  if (typeof value === 'string') return value.trim()
  if (Array.isArray(value)) return value.map(normalizeForCompare)
  if (isRecord(value)) {
    return Object.keys(value)
      .sort()
      .reduce<Record<string, unknown>>((out, key) => {
        out[key] = normalizeForCompare(value[key])
        return out
      }, {})
  }
  return value
}

function sameValue(left: unknown, right: unknown): boolean {
  return JSON.stringify(normalizeForCompare(left)) === JSON.stringify(normalizeForCompare(right))
}

function baselineValue(item: SettingsItem): unknown {
  return item.value !== null && item.value !== undefined ? item.value : item.default
}

function isSecretConfigured(item: SettingsItem): boolean {
  return item.value !== null && item.value !== undefined
}

function numberType(type: string): boolean {
  return ['int', 'integer', 'number', 'float', 'decimal', 'bigint'].includes(type)
}

function boolType(type: string): boolean {
  return type === 'bool' || type === 'boolean'
}

/**
 * Unknown registry keys still render (generic control chosen from the server
 * type) so a backend wave can add a field without silently hiding it from the
 * administrator. Known keys keep their designed label and validation.
 */
function genericField(item: SettingsItem): FieldConfig {
  const control = boolType(item.type)
    ? 'switch'
    : numberType(item.type)
      ? 'number'
      // An empty list is not evidence of a line-based control: `[].every(...)`
      // is vacuously true and previously rendered `site.oss_credits: []` as a
      // lines textarea (which loses the array-of-objects shape). Only a
      // non-empty all-string list can pick `lines`/`tags`; everything else
      // (including an empty list) uses the JSON control.
      : Array.isArray(item.default) &&
          item.default.length > 0 &&
          item.default.every((entry) => typeof entry === 'string')
        ? item.default.every((entry) => String(entry).includes('\n'))
          ? 'lines'
          : 'tags'
        : isRecord(item.default) || Array.isArray(item.default)
          ? 'json'
          : 'text'
  return { key: item.key, labelKey: '', control }
}

function parseLines(value: unknown): unknown {
  if (typeof value !== 'string') return value
  return value
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '')
}

function parseNumberList(value: unknown): unknown {
  if (typeof value !== 'string') return value
  return value
    .split(',')
    .map((entry) => entry.trim())
    .filter((entry) => entry !== '')
    .map((entry) => Number(entry))
}

/** Parses JSON text, returning the original string when it is invalid. */
function parseJson(value: unknown): unknown {
  if (typeof value !== 'string') return value
  try {
    return JSON.parse(value) as unknown
  } catch {
    return value
  }
}

function valuesOf(item: SettingsItem, draft: Draft): unknown {
  if (Object.prototype.hasOwnProperty.call(draft, item.key)) return draft[item.key]
  // Secrets are never echoed back into the input: the server value is a mask,
  // so an untouched secret field starts empty and contributes nothing to the
  // request unless the user types a replacement or clears it.
  if (item.secret) return undefined
  return baselineValue(item)
}

function validate(
  field: FieldConfig,
  value: unknown,
  t: (key: string, values?: Record<string, string | number>) => string,
): string | undefined {
  try {
    if (field.control === 'switch') {
      if (typeof value !== 'boolean') return t('settings.shell.field_invalid')
      return undefined
    }
    if (field.control === 'secret') {
      // `null` is an explicit clear; a string is a replacement value. Both are
      // valid request payloads, and an empty string never reaches this point.
      if (value === null) return undefined
      return typeof value === 'string' ? undefined : t('settings.shell.field_invalid')
    }
    if (field.control === 'number') {
      if (value === '' || value === null || value === undefined) {
        // Every numeric setting has a server default; sending an empty value
        // would be rejected by the registry type check, so block it locally.
        return field.required ? t('settings.shell.field_required') : t('settings.shell.field_invalid_number')
      }
      const parsed = typeof value === 'number' ? value : Number(String(value).trim())
      if (!Number.isFinite(parsed)) return t('settings.shell.field_invalid_number')
      if (field.integer === true && !Number.isInteger(parsed)) {
        return t('settings.shell.field_invalid_number')
      }
      if (field.greaterThan !== undefined && parsed <= field.greaterThan) {
        return t('settings.shell.field_number_min', { min: field.greaterThan })
      }
      if (field.min !== undefined && parsed < field.min) {
        return t('settings.shell.field_number_range', {
          min: field.min,
          max: field.max ?? '∞',
        })
      }
      if (field.max !== undefined && parsed > field.max) {
        return t('settings.shell.field_number_range', {
          min: field.min ?? 0,
          max: field.max,
        })
      }
      return undefined
    }
    if (field.control === 'tags' || field.control === 'lines') {
      const candidate = field.control === 'lines' ? parseLines(value) : value
      if (candidate === undefined || candidate === null) return undefined
      if (!Array.isArray(candidate) || candidate.some((entry) => typeof entry !== 'string')) {
        return t('settings.shell.field_invalid_list')
      }
      if (field.control === 'tags' && field.maxLength !== undefined) {
        const max = field.maxLength
        if (candidate.some((entry) => entry.length > max)) {
          return t('settings.shell.field_too_long', { max })
        }
      }
      return undefined
    }
    if (field.control === 'number_list') {
      // The control keeps raw text while the user types (`10,30`); parse the
      // same way here and at save time so a missing blur cannot send the raw
      // string to the server.
      const candidate = parseNumberList(value)
      if (!Array.isArray(candidate)) return t('settings.shell.field_invalid_list')
      if (
        candidate.some(
          (entry) => typeof entry !== 'number' || !Number.isFinite(entry) || !Number.isInteger(entry),
        )
      ) {
        return t('settings.shell.field_invalid_list')
      }
      return undefined
    }
    if (field.control === 'module_toggles') {
      return isRecord(value) ? undefined : t('settings.shell.field_invalid_json')
    }
    if (field.control === 'multi_select') {
      // An empty array is valid (the webhook then delivers nothing); entries
      // must belong to the server-declared closed set.
      if (
        !Array.isArray(value) ||
        value.some(
          (entry) =>
            typeof entry !== 'string' ||
            !(field.options ?? []).some((option) => String(option.value) === entry),
        )
      ) {
        return t('settings.shell.field_invalid_list')
      }
      return undefined
    }
    if (field.control === 'json') {
      const candidate = parseJson(value)
      return isRecord(candidate) || Array.isArray(candidate)
        ? undefined
        : t('settings.shell.field_invalid_json')
    }

    if (typeof value !== 'string') return t('settings.shell.field_invalid')
    const trimmed = value.trim()
    if (field.required === true && trimmed === '') {
      return t('settings.shell.field_required')
    }
    if (field.maxLength !== undefined && value.length > field.maxLength) {
      return t('settings.shell.field_too_long', { max: field.maxLength })
    }
    if (
      trimmed !== '' &&
      (field.key === 'site.server_address' ||
        field.key === 'site.logo_url' ||
        field.key === 'site.docs_url' ||
        field.key === 'site.status_page_url' ||
        field.key === 'site.seo.og_image_url')
    ) {
      // Absolute http(s) only: javascript:/data: URLs are rejected both here
      // and by the server's allowlist (F-08-01 validation table).
      if (!/^https?:\/\/[^\s]+$/i.test(trimmed)) return t('settings.shell.field_url')
    }
    if (field.key === 'notify.webhook.url' && trimmed !== '') {
      // UI §6.6 / ruling 08-3: webhook endpoints are https-only. The backend
      // allows an http draft while disabled, but the console never stores one.
      if (!/^https:\/\/[^\s]+$/i.test(trimmed)) return t('settings.shell.field_https_url')
    }
    return undefined
  } catch {
    return t('settings.shell.field_invalid')
  }
}

/**
 * One SettingsSection card (UI design §2). The component owns load, dirty,
 * save, 409, 403, field-error and secret-masking behaviour; the shell only
 * tracks which cards are dirty so it can guard navigation.
 */
export function SettingsSectionForm({
  config,
  readOnly,
  onDirtyChange,
}: SettingsSectionFormProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const query = useQuery({
    queryKey: ['settings-section', config.id],
    queryFn: ({ signal }) => fetchSettingsSection(config.id, signal),
  })

  const [draft, setDraft] = useState<Draft>({})
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>({})
  const [serverError, setServerError] = useState<string | null>(null)
  const [conflictOpen, setConflictOpen] = useState(false)
  const [pendingSave, setPendingSave] = useState<Record<string, unknown> | null>(null)
  const [saving, setSaving] = useState(false)
  const idempotencyRef = useRef<string>(createIdempotencyKey())
  const titleId = useId()

  const data = query.data

  // A fresh server read (after save or manual refresh) invalidates the local
  // draft: keeping local overrides across a new baseline would make the card
  // dirty forever and let a stale value overwrite the refreshed one.
  const updatedAt = data?.updated_at ?? null
  useEffect(() => {
    setDraft({})
    setFieldErrors({})
    setServerError(null)
  }, [updatedAt])

  const items = useMemo(() => data?.items ?? [], [data])
  const itemByKey = useMemo(() => {
    const map = new Map<string, SettingsItem>()
    for (const item of items) map.set(item.key, item)
    return map
  }, [items])

  const fields = useMemo(() => {
    const known = new Set(config.fields.map((field) => field.key))
    const configured = config.fields.filter((field) => itemByKey.has(field.key))
    const generic = items.filter((item) => !known.has(item.key)).map(genericField)
    return [...configured, ...generic]
  }, [config.fields, itemByKey, items])

  const changedKeys = useMemo(() => {
    const keys: string[] = []
    for (const [key, value] of Object.entries(draft)) {
      const item = itemByKey.get(key)
      if (item === undefined) continue
      if (item.secret) {
        // `null` is the explicit clear instruction; a typed string is a new
        // value. An empty string never enters the draft.
        if (value === null || (typeof value === 'string' && value !== '')) keys.push(key)
        continue
      }
      if (!sameValue(value, baselineValue(item))) keys.push(key)
    }
    return keys
  }, [draft, itemByKey])

  const dirty = changedKeys.length > 0

  useEffect(() => {
    onDirtyChange(config.id, dirty)
  }, [config.id, dirty, onDirtyChange])

  // Unmounting (tab switch) must release the shell-level dirty flag, otherwise
  // the navigation guard would keep blocking after the user discarded a tab.
  useEffect(() => {
    return () => {
      onDirtyChange(config.id, false)
    }
  }, [config.id, onDirtyChange])

  function updateValue(key: string, value: unknown): void {
    setDraft((current) => {
      const next = { ...current }
      if (value === '' && itemByKey.get(key)?.secret === true) {
        delete next[key]
      } else {
        next[key] = value
      }
      return next
    })
    setFieldErrors((current) => {
      if (!(key in current)) return current
      const next = { ...current }
      delete next[key]
      return next
    })
    setServerError(null)
  }

  function discardDraft(): void {
    setDraft({})
    setFieldErrors({})
    setServerError(null)
  }

  function collectDirtyValues(): Record<string, unknown> {
    const values: Record<string, unknown> = {}
    for (const key of changedKeys) {
      const item = itemByKey.get(key)
      if (item === undefined) continue
      let value = draft[key]
      if (item.secret && (value === '' || value === undefined)) continue
      // Parse raw text shadows at save time: a click on Save blurs the input
      // first, but treating the parse as the request boundary makes the body
      // independent of event ordering (F3).
      const field = fields.find((entry) => entry.key === key)
      if (field?.control === 'lines') value = parseLines(value)
      if (field?.control === 'number_list') value = parseNumberList(value)
      if (field?.control === 'json') value = parseJson(value)
      values[key] = value
    }
    return values
  }

  function requestSave(): void {
    if (readOnly) return
    const values = collectDirtyValues()
    if (Object.keys(values).length === 0) return

    const validationErrors: FieldErrors = {}
    // Validate only keys that will actually be sent. A field the user edited
    // and then reverted is absent from `values`; validating it would feed
    // `undefined` into the checks and block an otherwise valid save (F2).
    for (const [key, value] of Object.entries(values)) {
      const field = fields.find((entry) => entry.key === key)
      if (field === undefined) continue
      const error = validate(field, value, t)
      if (error !== undefined) validationErrors[key] = error
    }

    // Cross-field pre-checks from UI design §6.2. The server keeps enforcing
    // these; the client mirrors them so the error lands on the right field.
    const effective = (key: string): unknown => {
      if (Object.prototype.hasOwnProperty.call(values, key)) return values[key]
      const item = itemByKey.get(key)
      return item === undefined ? undefined : baselineValue(item)
    }

    if (config.id === 'auth') {
      const signup = effective('auth.signup_enabled')
      const password = effective('auth.password_login_enabled')
      if (signup === false && password === false) {
        validationErrors['auth.password_login_enabled'] ??= t(
          'settings.shell.cross_at_least_one_sign_in',
        )
      }
    }

    if (config.id === 'auth.turnstile') {
      const enabled = effective('auth.turnstile.enabled') === true
      const secretItem = itemByKey.get('auth.turnstile.secret_key')
      if (enabled && secretItem !== undefined) {
        const candidate = values['auth.turnstile.secret_key']
        const typed =
          typeof candidate === 'string' && candidate !== ''
        const configured =
          isSecretConfigured(secretItem) && candidate !== null
        if (!typed && !configured) {
          validationErrors['auth.turnstile.secret_key'] ??= t(
            'settings.shell.cross_turnstile_secret',
          )
        }
      }
    }

    if (config.id === 'notify.webhook') {
      // Backend D2-11 only demands a complete endpoint while enabled=true;
      // a disabled draft may stay empty. Mirroring that split keeps a valid
      // off-switch save from being blocked by empty fields.
      const enabled = effective('notify.webhook.enabled') === true
      if (enabled) {
        const url = effective('notify.webhook.url')
        if (typeof url !== 'string' || url.trim() === '') {
          validationErrors['notify.webhook.url'] ??= t('settings.shell.cross_webhook_url')
        } else if (!/^https:\/\/[^\s]+$/i.test(url.trim())) {
          validationErrors['notify.webhook.url'] ??= t('settings.shell.field_https_url')
        }

        const secretItem = itemByKey.get('notify.webhook.secret')
        const candidate = values['notify.webhook.secret']
        const typed = typeof candidate === 'string' && candidate !== ''
        const configured =
          secretItem !== undefined && isSecretConfigured(secretItem) && candidate !== null
        if (!typed && !configured) {
          validationErrors['notify.webhook.secret'] ??= t(
            'settings.shell.cross_webhook_secret',
          )
        }
      }
    }

    if (Object.keys(validationErrors).length > 0) {
      setFieldErrors(validationErrors)
      const firstKey = Object.keys(validationErrors)[0]
      if (firstKey !== undefined) {
        document.getElementById(`setting-${config.id}-${firstKey}`)?.focus()
      }
      return
    }

    // Confirmation is required only in the risky direction (F5). The server's
    // `danger_direction` is authoritative once present; the local table is a
    // temporary fallback for backends that have not shipped the field yet.
    const dangerous = Object.entries(values).some(([key, value]) => {
      const item = itemByKey.get(key)
      if (item === undefined || !item.dangerous) return false
      const declared = item.danger_direction
      // `any` is an explicit server verdict, not a missing value: map it to
      // the local `change` semantics so the fallback table cannot override it.
      const serverDirection =
        declared === 'off' || declared === 'on'
          ? declared
          : declared === 'any'
            ? ('change' as const)
            : undefined
      const direction =
        serverDirection ??
        fields.find((entry) => entry.key === key)?.dangerDirection ??
        'change'
      // The server's `on`/`off` vocabulary describes the *boolean* direction.
      // For non-boolean keys (strings, numbers, lists) the backend treats any
      // change as risky (`settings/service.go`: non-boolean values cannot be
      // compared to `true`), so interpreting `on` as "value becomes true"
      // would never ask for confirmation and the PUT would 400 on
      // `dangerous_confirmed` being absent. Every changed key in `values` is
      // already a real change, so any non-boolean edited value is risky.
      const booleanKey = boolType(item.type)
      if (!booleanKey) return true
      if (direction === 'off') return value === false
      if (direction === 'on') return value === true
      return !sameValue(value, baselineValue(item))
    })
    if (dangerous) {
      setPendingSave(values)
      return
    }
    void performSave(values)
  }

  async function performSave(values: Record<string, unknown>, confirmed = false): Promise<void> {
    if (data === undefined) return
    setSaving(true)
    setServerError(null)
    try {
      await saveSettingsSection(
        {
          section: config.id,
          values,
          expect_updated_at: data.updated_at,
          ...(confirmed ? { dangerous_confirmed: true as const } : {}),
        },
        idempotencyRef.current,
      )
      // One key per user intent: a repeat save (new intent) gets a new key.
      idempotencyRef.current = createIdempotencyKey()
      push({ tone: 'success', message: t('settings.shell.saved', { section: t(config.titleKey) }) })
      discardDraft()
      await queryClient.invalidateQueries({ queryKey: ['settings-section', config.id] })
    } catch (error) {
      if (error instanceof ApiError && error.kind === 'conflict') {
        setConflictOpen(true)
      } else if (error instanceof ApiError && error.kind === 'forbidden') {
        setServerError(t('settings.shell.save_forbidden'))
      } else if (error instanceof ApiError && error.kind === 'rate_limited') {
        push({ tone: 'error', message: t('settings.shell.rate_limited') })
      } else {
        const errors = fieldErrorsOf(error)
        if (errors.length > 0) {
          const next: FieldErrors = {}
          for (const entry of errors) {
            next[entry.field] = entry.message !== '' ? entry.message : t('settings.shell.field_invalid')
          }
          setFieldErrors(next)
        } else {
          setServerError(errorMessage(error))
        }
        // Keep the draft and the same idempotency key: the user is retrying
        // the same intent, so the server can replay instead of duplicating.
      }
    } finally {
      setSaving(false)
    }
  }

  function refreshAfterConflict(): void {
    discardDraft()
    setConflictOpen(false)
    setPendingSave(null)
    void query.refetch()
  }

  const sectionTitle = t(config.titleKey)

  // The test endpoint reads the saved snapshot, so the button is enabled only
  // when the stored URL is non-empty; an unsaved draft is not testable.
  const webhookUrlItem = itemByKey.get('notify.webhook.url')
  const storedWebhookUrl = webhookUrlItem === undefined ? '' : baselineValue(webhookUrlItem)
  const webhookUrlConfigured =
    typeof storedWebhookUrl === 'string' && storedWebhookUrl.trim() !== ''

  return (
    <section
      data-testid={`settings-section-${config.id}`}
      aria-labelledby={titleId}
      className="rounded-xl border border-border-default bg-surface-card p-5"
    >
      <header className="flex items-start justify-between gap-4 border-b border-border-hairline pb-3">
        <div>
          <h2 id={titleId} className="text-[15px] font-semibold leading-[22px] text-ink-1">
            {sectionTitle}
          </h2>
          {config.descriptionKey !== undefined ? (
            <p className="mt-0.5 text-xs text-ink-3">{t(config.descriptionKey)}</p>
          ) : null}
        </div>
        <span className="font-mono text-[11px] text-ink-3" aria-hidden="true">
          {config.id}.*
        </span>
        {dirty ? (
          <span
            aria-label={t('settings.shell.dirty')}
            title={t('settings.shell.dirty')}
            className="mt-1 size-2 shrink-0 rounded-full bg-warning"
          />
        ) : null}
      </header>

      {query.isPending ? (
        <div className="space-y-3 py-4" aria-hidden="true">
          {[0, 1, 2].map((row) => (
            <div key={row} className="flex items-center gap-4">
              <div className="h-4 w-40 rounded bg-surface-subtle" />
              <div className="h-10 flex-1 rounded-lg bg-surface-subtle" />
            </div>
          ))}
        </div>
      ) : null}

      {query.isError ? (
        <div className="py-4">
          {query.error instanceof ApiError && query.error.kind === 'forbidden' ? (
            <FormAlert tone="danger">{t('settings.shell.forbidden')}</FormAlert>
          ) : (
            <div className="space-y-3">
              <FormAlert tone="danger">
                {t('settings.shell.load_error')} {errorMessage(query.error)}
              </FormAlert>
              <Button variant="secondary" onClick={() => void query.refetch()}>
                {t('settings.shell.retry')}
              </Button>
            </div>
          )}
        </div>
      ) : null}

      {query.isSuccess ? (
        <>
          {serverError !== null ? (
            <div className="pt-4">
              <FormAlert tone="danger">{serverError}</FormAlert>
            </div>
          ) : null}

          <div className="space-y-5 py-4">
            {fields.map((field) => {
              const item = itemByKey.get(field.key)
              if (item === undefined) return null
              return (
                <FieldRow
                  key={field.key}
                  sectionId={config.id}
                  field={field}
                  item={item}
                  value={valuesOf(item, draft)}
                  error={fieldErrors[field.key]}
                  disabled={readOnly || field.locked === true || saving}
                  locked={field.locked === true}
                  onChange={(value) => updateValue(field.key, value)}
                />
              )
            })}
            {fields.length === 0 ? (
              <p className="text-sm text-ink-3">{t('settings.shell.load_error')}</p>
            ) : null}
          </div>

          <footer className="flex flex-col gap-3 border-t border-border-hairline pt-4">
            {config.testKind === 'webhook' && !readOnly ? (
              <WebhookTestPanel
                disabled={saving}
                urlConfigured={webhookUrlConfigured}
              />
            ) : null}
            <div className="flex items-center justify-end gap-2">
              {dirty && !readOnly ? (
                <Button variant="ghost" onClick={discardDraft} disabled={saving}>
                  {t('settings.shell.discard')}
                </Button>
              ) : null}
              {!readOnly ? (
                <Button onClick={requestSave} disabled={!dirty || saving}>
                  {saving ? t('settings.shell.saving') : t('settings.shell.save')}
                </Button>
              ) : null}
            </div>
          </footer>
        </>
      ) : null}

      <Dialog
        open={conflictOpen}
        onOpenChange={(open) => {
          if (!open) setConflictOpen(false)
        }}
        title={t('settings.shell.conflict_title')}
        description={t('settings.shell.conflict_body')}
        role="alertdialog"
        footer={
          <Button onClick={refreshAfterConflict}>{t('settings.shell.conflict_refresh')}</Button>
        }
      />

      <Dialog
        open={pendingSave !== null}
        onOpenChange={(open) => {
          if (!open) setPendingSave(null)
        }}
        title={t('settings.shell.danger_title')}
        description={t('settings.shell.danger_body')}
        role="alertdialog"
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="secondary" onClick={() => setPendingSave(null)}>
              {t('settings.shell.danger_cancel')}
            </Button>
            <Button
              variant="danger"
              onClick={() => {
                const values = pendingSave
                setPendingSave(null)
                if (values !== null) void performSave(values, true)
              }}
            >
              {t('settings.shell.danger_confirm')}
            </Button>
          </div>
        }
      />
    </section>
  )
}

interface WebhookTestPanelProps {
  disabled: boolean
  /** The saved URL is non-empty; the endpoint is not testable otherwise. */
  urlConfigured: boolean
}

type TestTone = 'success' | 'danger' | 'warning'

/**
 * "Send test event" action for the Webhook section (UI §6.6 / F-08-20).
 *
 * The backend tests the **saved** configuration, ignores `enabled=false` and
 * the event subscription, and rate-limits to 3 calls/minute per administrator.
 * A 429 drives the inline countdown from `Retry-After`; every other result
 * (including a non-2xx target response, which is still HTTP 200) renders in
 * place and never touches the settings draft.
 */
function WebhookTestPanel({ disabled, urlConfigured }: WebhookTestPanelProps) {
  const { t } = useI18n()
  const countdown = useCountdown()
  const [sending, setSending] = useState(false)
  const [result, setResult] = useState<{ tone: TestTone; message: string } | null>(null)

  const blocked = disabled || sending || countdown.running || !urlConfigured

  async function send(): Promise<void> {
    setSending(true)
    setResult(null)
    try {
      const response = await testWebhook()
      setResult({
        tone: response.ok ? 'success' : 'danger',
        message: t('settings.webhook.test.result', { code: response.status_code }),
      })
    } catch (error) {
      if (error instanceof ApiError) {
        if (error.kind === 'rate_limited') {
          // Server-declared window; never invent a duration.
          const seconds = error.retry_after ?? 60
          countdown.start(seconds)
          setResult({
            tone: 'warning',
            message: t('settings.webhook.test.rate_limited'),
          })
        } else if (error.status === 422) {
          setResult({
            tone: 'danger',
            message: t('settings.webhook.test.not_configured'),
          })
        } else if (error.status === 504) {
          setResult({
            tone: 'danger',
            message: t('settings.webhook.test.timeout'),
          })
        } else if (error.kind === 'offline') {
          setResult({
            tone: 'danger',
            message: t('settings.webhook.test.offline'),
          })
        } else {
          setResult({
            tone: 'danger',
            message: t('settings.webhook.test.failed', {
              reason: error.detail ?? error.title,
            }),
          })
        }
      } else {
        setResult({
          tone: 'danger',
          message: t('settings.webhook.test.failed', {
            reason: error instanceof Error ? error.message : String(error),
          }),
        })
      }
    } finally {
      setSending(false)
    }
  }

  const label = countdown.running
    ? t('settings.webhook.test.retry_in', { seconds: countdown.seconds })
    : sending
      ? t('settings.webhook.test.testing')
      : t('settings.webhook.test.button')

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p id="webhook-test-hint" className="text-xs text-ink-3">
          {urlConfigured
            ? t('settings.webhook.test.hint')
            : t('settings.webhook.test.url_required')}
        </p>
        <Button
          variant="secondary"
          onClick={() => void send()}
          disabled={blocked}
          aria-describedby="webhook-test-hint"
        >
          {label}
        </Button>
      </div>
      {result !== null ? <FormAlert tone={result.tone}>{result.message}</FormAlert> : null}
    </div>
  )
}

interface FieldRowProps {
  sectionId: string
  field: FieldConfig
  item: SettingsItem
  value: unknown
  error?: string
  disabled: boolean
  locked: boolean
  onChange: (value: unknown) => void
}

function FieldRow({
  sectionId,
  field,
  item,
  value,
  error,
  disabled,
  locked,
  onChange,
}: FieldRowProps) {
  const { t } = useI18n()
  const label = field.labelKey === '' ? field.key : t(field.labelKey)
  const hint =
    locked && field.hintKey === undefined
      ? t('settings.shell.field_locked')
      : field.hintKey !== undefined
        ? t(field.hintKey)
        : undefined
  const inputId = `setting-${sectionId}-${field.key}`
  const hintId = `${inputId}-hint`
  const errorId = `${inputId}-error`
  // Only describe the hint while it is actually rendered: when an error is
  // present the hint paragraph disappears, so pointing aria-describedby at it
  // would reference a missing id.
  const describedBy =
    error !== undefined ? errorId : hint !== undefined ? hintId : undefined

  if (field.control === 'switch') {
    return (
      <div className="flex flex-col gap-1">
        <div className="flex items-center justify-between gap-6">
          <label htmlFor={inputId} className="text-[13px] font-medium text-ink-1">
            {label}
          </label>
          <input
            id={inputId}
            type="checkbox"
            role="switch"
            aria-checked={value === true}
            aria-invalid={error !== undefined ? true : undefined}
            aria-describedby={describedBy}
            aria-errormessage={error !== undefined ? errorId : undefined}
            disabled={disabled}
            checked={value === true}
            onChange={(event) => onChange(event.target.checked)}
            className="h-5 w-9 appearance-none rounded-full border border-border-default bg-surface-subtle transition-colors checked:border-brand checked:bg-brand disabled:cursor-not-allowed disabled:opacity-50"
          />
        </div>
        {hint !== undefined && error === undefined ? (
          <p id={hintId} className="text-xs text-ink-3">{hint}</p>
        ) : null}
        {error !== undefined ? (
          <p id={errorId} className="text-xs text-danger">{error}</p>
        ) : null}
      </div>
    )
  }

  if (field.control === 'select') {
    return (
      <div className="flex flex-col gap-1.5">
        <label htmlFor={inputId} className="text-[13px] font-medium text-ink-1">
          {label}
        </label>
        <select
          id={inputId}
          value={typeof value === 'string' ? value : ''}
          disabled={disabled}
          aria-invalid={error !== undefined ? true : undefined}
          aria-describedby={describedBy}
          aria-errormessage={error !== undefined ? errorId : undefined}
          onChange={(event) => onChange(event.target.value)}
          className="h-10 w-full rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
        >
          <option value="">—</option>
          {(field.options ?? []).map((option) => (
            <option key={String(option.value)} value={String(option.value)}>
              {t(option.labelKey)}
            </option>
          ))}
          {typeof value === 'string' &&
          value !== '' &&
          !(field.options ?? []).some((option) => String(option.value) === value) ? (
            <option value={value}>{value}</option>
          ) : null}
        </select>
        {hint !== undefined && error === undefined ? (
          <p id={hintId} className="text-xs text-ink-3">{hint}</p>
        ) : null}
        {error !== undefined ? (
          <p id={errorId} className="text-xs text-danger">{error}</p>
        ) : null}
      </div>
    )
  }

  if (field.control === 'secret') {
    return (
      <SecretField
        id={inputId}
        label={label}
        value={value}
        configured={isSecretConfigured(item)}
        hint={hint}
        error={error}
        disabled={disabled}
        multiline={field.multiline === true}
        onChange={onChange}
      />
    )
  }

  if (field.control === 'module_toggles') {
    const record = isRecord(value) ? value : {}
    const toggles = field.toggles ?? Object.keys(record).map((key) => ({ key, labelKey: '' }))
    return (
      <fieldset
        className="flex flex-col gap-1.5"
        disabled={disabled}
        aria-describedby={describedBy}
        aria-errormessage={error !== undefined ? errorId : undefined}
      >
        <legend className="text-[13px] font-medium text-ink-1">{label}</legend>
        <div className="flex flex-wrap gap-x-5 gap-y-2">
          {toggles.map((entry) => {
            const toggleId = `${inputId}-${entry.key}`
            return (
              <label key={entry.key} htmlFor={toggleId} className="flex items-center gap-2 text-sm text-ink-2">
                <input
                  id={toggleId}
                  type="checkbox"
                  checked={record[entry.key] === true}
                  disabled={disabled}
                  onChange={(event) => onChange({ ...record, [entry.key]: event.target.checked })}
                />
                {entry.labelKey === '' ? entry.key : t(entry.labelKey)}
              </label>
            )
          })}
        </div>
        {hint !== undefined && error === undefined ? (
          <p id={hintId} className="text-xs text-ink-3">{hint}</p>
        ) : null}
        {error !== undefined ? (
          <p id={errorId} className="text-xs text-danger">{error}</p>
        ) : null}
      </fieldset>
    )
  }

  if (field.control === 'multi_select') {
    const selected = Array.isArray(value) ? value.map(String) : []
    const options = field.options ?? []
    return (
      <fieldset
        className="flex flex-col gap-1.5"
        disabled={disabled}
        aria-invalid={error !== undefined ? true : undefined}
        aria-describedby={describedBy}
        aria-errormessage={error !== undefined ? errorId : undefined}
      >
        <legend className="text-[13px] font-medium text-ink-1">{label}</legend>
        <div className="flex flex-wrap gap-x-5 gap-y-2">
          {options.map((option) => {
            const optionId = `${inputId}-${String(option.value)}`
            const checked = selected.includes(String(option.value))
            return (
              <label
                key={String(option.value)}
                htmlFor={optionId}
                className="flex items-center gap-2 text-sm text-ink-2"
              >
                <input
                  id={optionId}
                  type="checkbox"
                  checked={checked}
                  disabled={disabled}
                  onChange={(event) =>
                    onChange(
                      event.target.checked
                        ? // Keep the server's option order in the payload so
                          // the saved order stays stable regardless of click order.
                          options
                            .map((entry) => String(entry.value))
                            .filter(
                              (entry) =>
                                entry === String(option.value) || selected.includes(entry),
                            )
                        : selected.filter((entry) => entry !== String(option.value)),
                    )
                  }
                />
                {t(option.labelKey)}
              </label>
            )
          })}
        </div>
        {selected.length === 0 && field.emptyHintKey !== undefined && error === undefined ? (
          <p role="status" className="text-xs text-warning">{t(field.emptyHintKey)}</p>
        ) : null}
        {hint !== undefined && error === undefined ? (
          <p id={hintId} className="text-xs text-ink-3">{hint}</p>
        ) : null}
        {error !== undefined ? (
          <p id={errorId} className="text-xs text-danger">{error}</p>
        ) : null}
      </fieldset>
    )
  }

  if (field.control === 'json' || field.control === 'textarea') {
    const text =
      field.control === 'json'
        ? typeof value === 'string'
          ? value
          : JSON.stringify(value ?? {}, null, 2)
        : typeof value === 'string'
          ? value
          : ''
    return (
      <div className="flex flex-col gap-1.5">
        <label htmlFor={inputId} className="text-[13px] font-medium text-ink-1">
          {label}
        </label>
        <textarea
          id={inputId}
          value={text}
          disabled={disabled}
          rows={field.control === 'json' ? 5 : 4}
          aria-invalid={error !== undefined ? true : undefined}
          aria-describedby={describedBy}
          aria-errormessage={error !== undefined ? errorId : undefined}
          onChange={(event) => onChange(event.target.value)}
          onBlur={
            field.control === 'json'
              ? () => onChange(parseJson(text))
              : undefined
          }
          className="w-full rounded-lg border border-border-default bg-surface-card px-3 py-2 font-mono text-[13px] text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
        />
        {hint !== undefined && error === undefined ? (
          <p id={hintId} className="text-xs text-ink-3">{hint}</p>
        ) : null}
        {error !== undefined ? (
          <p id={errorId} className="text-xs text-danger">{error}</p>
        ) : null}
      </div>
    )
  }

  if (field.control === 'lines') {
    // Raw text is kept in the draft while typing; parsing happens on blur and
    // again at save time. Parsing per keystroke swallowed Enter and merged
    // words across lines (F3).
    const text = typeof value === 'string' ? value : Array.isArray(value) ? value.map(String).join('\n') : ''
    return (
      <div className="flex flex-col gap-1.5">
        <label htmlFor={inputId} className="text-[13px] font-medium text-ink-1">
          {label}
        </label>
        <textarea
          id={inputId}
          value={text}
          disabled={disabled}
          rows={5}
          aria-invalid={error !== undefined ? true : undefined}
          aria-describedby={describedBy}
          aria-errormessage={error !== undefined ? errorId : undefined}
          onChange={(event) => onChange(event.target.value)}
          onBlur={() => onChange(parseLines(text))}
          className="w-full rounded-lg border border-border-default bg-surface-card px-3 py-2 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
        />
        {hint !== undefined && error === undefined ? (
          <p id={hintId} className="text-xs text-ink-3">{hint}</p>
        ) : null}
        {error !== undefined ? (
          <p id={errorId} className="text-xs text-danger">{error}</p>
        ) : null}
      </div>
    )
  }

  if (field.control === 'tags') {
    const tags = Array.isArray(value) ? value.map(String) : []
    return (
      <TagInput
        id={inputId}
        label={label}
        hint={hint}
        error={error}
        disabled={disabled}
        values={tags}
        onChange={onChange}
      />
    )
  }

  if (field.control === 'number_list') {
    // Comma-separated raw text until blur/Enter/save; parsing per keystroke
    // turned `10,30` into `1030` because the comma never stayed in the input.
    const text = typeof value === 'string' ? value : Array.isArray(value) ? value.map(String).join(', ') : ''
    return (
      <TextField
        id={inputId}
        label={label}
        type="text"
        inputMode="numeric"
        value={text}
        disabled={disabled}
        hint={hint}
        error={error}
        onChange={(event) => onChange(event.target.value)}
        onBlur={() => onChange(parseNumberList(text))}
        onKeyDown={(event) => {
          if (event.key === 'Enter') {
            event.preventDefault()
            onChange(parseNumberList(text))
          }
        }}
      />
    )
  }

  if (field.control === 'number') {
    return (
      <TextField
        id={inputId}
        label={label}
        type="number"
        inputMode="numeric"
        value={value === null || value === undefined ? '' : String(value)}
        disabled={disabled}
        min={field.min}
        max={field.max}
        step={field.integer === false ? 'any' : 1}
        hint={hint}
        error={error}
        onChange={(event) => {
          const raw = event.target.value
          onChange(raw === '' ? '' : Number(raw))
        }}
      />
    )
  }

  return (
    <TextField
      id={inputId}
      label={label}
      value={typeof value === 'string' ? value : ''}
      disabled={disabled}
      maxLength={field.maxLength}
      hint={hint}
      error={error}
      onChange={(event) => onChange(event.target.value)}
    />
  )
}

interface SecretFieldProps {
  id: string
  label: string
  value: unknown
  configured: boolean
  hint?: string
  error?: string
  disabled: boolean
  multiline: boolean
  onChange: (value: unknown) => void
}

/**
 * Secret inputs never echo the stored value: the server sends a mask, and the
 * field starts empty. Only a typed value (or an explicit clear) enters the
 * request body (UI design §3).
 */
function SecretField({
  id,
  label,
  value,
  configured,
  hint,
  error,
  disabled,
  multiline,
  onChange,
}: SecretFieldProps) {
  const { t } = useI18n()
  const [visible, setVisible] = useState(false)
  const text = typeof value === 'string' ? value : ''
  const clearRequested = value === null
  const hintId = `${id}-hint`
  const errorId = `${id}-error`
  // Only describe the hint while it is actually rendered: when an error is
  // present the hint paragraph disappears, so pointing aria-describedby at it
  // would reference a missing id.
  const describedBy =
    error !== undefined ? errorId : hint !== undefined ? hintId : undefined
  const common = {
    id,
    value: text,
    disabled,
    autoComplete: 'off' as const,
    'aria-invalid': error !== undefined ? true : undefined,
    'aria-describedby': describedBy,
    'aria-errormessage': error !== undefined ? errorId : undefined,
    placeholder: configured && !clearRequested ? t('settings.shell.secret_placeholder') : undefined,
    onChange: (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) =>
      onChange(event.target.value),
    className:
      'w-full rounded-lg border bg-surface-card px-3 text-sm text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50 ' +
      (error !== undefined ? 'border-danger' : 'border-border-default'),
  }

  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-center justify-between gap-3">
        <label htmlFor={id} className="text-[13px] font-medium text-ink-1">
          {label}
        </label>
        <span className="flex items-center gap-2">
          <StatusBadge variant={configured && !clearRequested ? 'success' : 'neutral-outline'}>
            {configured && !clearRequested
              ? t('settings.shell.secret_configured')
              : t('settings.shell.secret_not_configured')}
          </StatusBadge>
          {configured && !clearRequested && !disabled ? (
            <button
              type="button"
              className="text-xs text-danger underline"
              onClick={() => {
                if (globalThis.confirm(t('settings.shell.secret_clear_confirm'))) onChange(null)
              }}
            >
              {t('settings.shell.secret_clear')}
            </button>
          ) : null}
        </span>
      </div>
      <div className="relative">
        {multiline ? (
          <textarea
            {...common}
            rows={4}
            className={`${common.className} py-2 font-mono`}
          />
        ) : (
          <input
            {...common}
            className={`${common.className} h-10 pr-11`}
            type={visible ? 'text' : 'password'}
          />
        )}
        <button
          type="button"
          aria-label={visible ? t('settings.shell.secret_hide') : t('settings.shell.secret_show')}
          aria-pressed={visible}
          disabled={disabled}
          onClick={() => setVisible((current) => !current)}
          className="absolute inset-y-0 right-0 flex w-10 items-center justify-center text-ink-3 hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-50"
        >
          {visible ? '◎' : '◉'}
        </button>
      </div>
      {error === undefined ? (
        <p id={hintId} className="text-xs text-ink-3">{hint ?? t('settings.shell.secret_hint')}</p>
      ) : null}
      {error !== undefined ? (
        <p id={errorId} className="text-xs text-danger">{error}</p>
      ) : null}
    </div>
  )
}

interface TagInputProps {
  id: string
  label: string
  hint?: string
  error?: string
  disabled: boolean
  values: string[]
  onChange: (value: unknown) => void
}

function TagInput({ id, label, hint, error, disabled, values, onChange }: TagInputProps) {
  const [pending, setPending] = useState('')
  const hintId = `${id}-hint`
  const errorId = `${id}-error`
  // Only describe the hint while it is actually rendered: when an error is
  // present the hint paragraph disappears, so pointing aria-describedby at it
  // would reference a missing id.
  const describedBy =
    error !== undefined ? errorId : hint !== undefined ? hintId : undefined

  function commit(raw: string): void {
    const entries = raw
      .split(/[\n,]/)
      .map((entry) => entry.trim())
      .filter((entry) => entry !== '')
    if (entries.length === 0) return
    onChange([...new Set([...values, ...entries])])
    setPending('')
  }

  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={id} className="text-[13px] font-medium text-ink-1">
        {label}
      </label>
      {values.length > 0 ? (
        <ul className="flex flex-wrap gap-1.5" aria-label={label}>
          {values.map((entry) => (
            <li
              key={entry}
              className="inline-flex items-center gap-1 rounded-md border border-border-hairline bg-surface-subtle px-2 py-0.5 text-xs text-ink-1"
            >
              <span className="font-mono">{entry}</span>
              <button
                type="button"
                aria-label={`${label}: ${entry}`}
                disabled={disabled}
                className="text-ink-3 hover:text-danger"
                onClick={() => onChange(values.filter((value) => value !== entry))}
              >
                ×
              </button>
            </li>
          ))}
        </ul>
      ) : null}
      <input
        id={id}
        value={pending}
        disabled={disabled}
        aria-invalid={error !== undefined ? true : undefined}
        aria-describedby={describedBy}
        aria-errormessage={error !== undefined ? errorId : undefined}
        placeholder={hint}
        onChange={(event) => {
          const raw = event.target.value
          if (raw.includes(',')) commit(raw)
          else setPending(raw)
        }}
        onBlur={() => commit(pending)}
        onKeyDown={(event) => {
          if (event.key === 'Enter') {
            event.preventDefault()
            commit(pending)
          }
        }}
        className="h-10 w-full rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
      />
      {hint !== undefined && error === undefined ? (
        <p id={hintId} className="text-xs text-ink-3">{hint}</p>
      ) : null}
      {error !== undefined ? (
        <p id={errorId} className="text-xs text-danger">{error}</p>
      ) : null}
    </div>
  )
}
