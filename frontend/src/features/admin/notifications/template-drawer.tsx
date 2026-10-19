import { useEffect, useMemo, useRef, useState, type RefObject } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert } from '@/shared/ui/form'
import { Tag } from '@/shared/ui/tag'
import { useToastStore } from '@/shared/ui/toast'

import {
  fieldForProblem,
  getTemplate,
  previewTemplate,
  updateTemplate,
} from './api'
import { PreviewDialog } from './preview-dialog'
import {
  BODY_MAX_LENGTH,
  CTA_TEXT_MAX_LENGTH,
  SUBJECT_MAX_LENGTH,
  categoryLabelKey,
  isLockedTemplate,
  scanTemplateVariables,
  variablesFor,
} from './registry'
import { NOTIFICATION_LOCALES, type NotificationLocale, type NotificationTemplate } from './types'
import { useOnlineStatus } from './use-online'
import { insertAtCursor, isValidCtaUrl, runeLength } from './utils'

export interface TemplateDrawerProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** 打开抽屉时的行（来自列表）；null 时抽屉不渲染内容。 */
  template: NotificationTemplate | null
  canManage: boolean
  /** 保存成功后让列表重新读取权威行。 */
  onSaved: (updated: NotificationTemplate) => void
}

type DraftField = 'subject' | 'body' | 'cta_text' | 'cta_url'

interface Draft {
  subject: string
  body: string
  cta_text: string
  cta_url: string
  enabled: boolean
}

type FieldErrors = Partial<Record<DraftField | 'variables' | 'enabled', string>>

function baselineOf(template: NotificationTemplate): Draft {
  return {
    subject: template.subject,
    body: template.body,
    cta_text: template.cta_text ?? '',
    cta_url: template.cta_url ?? '',
    enabled: template.enabled,
  }
}

function sameDraft(left: Draft, right: Draft): boolean {
  return (
    left.subject === right.subject &&
    left.body === right.body &&
    left.cta_text === right.cta_text &&
    left.cta_url === right.cta_url &&
    left.enabled === right.enabled
  )
}

function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : String(error)
}

function isOfflineError(error: unknown): boolean {
  return error instanceof ApiError && error.kind === 'offline'
}

/**
 * 模板编辑抽屉（§6.9，560px 右侧滑入）。
 *
 * 关键口径：
 * - key / channel 不可改；语言切换是切到同一 key×channel 的另一行，因此
 *   语言变化会重新 GET 详情（同时充当列表筛选之外的「跨语言编辑」入口）；
 * - 锁定模板（auth.* / security.alert / system.smtp_test / system.announcement）
 *   不渲染开关，服务端仍以 422 locked_template 兜底；
 * - 保存始终携带 `expect_updated_at`（乐观锁），409 提示刷新；
 * - 变量面板是唯一插入方式，未登记/非法构造即时标红并禁用保存。
 */
export function TemplateDrawer({
  open,
  onOpenChange,
  template,
  canManage,
  onSaved,
}: TemplateDrawerProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const online = useOnlineStatus()

  const [locale, setLocale] = useState<NotificationLocale>('en')
  const [draft, setDraft] = useState<Draft | null>(null)
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>({})
  const [saveError, setSaveError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [conflictOpen, setConflictOpen] = useState(false)
  const [pendingLocale, setPendingLocale] = useState<NotificationLocale | null>(null)
  const [pendingDisable, setPendingDisable] = useState(false)
  const [previewOpen, setPreviewOpen] = useState(false)
  const [preview, setPreview] = useState<{ subject: string; body: string } | null>(null)
  const [previewError, setPreviewError] = useState<string | null>(null)
  const [previewLoading, setPreviewLoading] = useState(false)
  const [activeField, setActiveField] = useState<DraftField>('body')

  // Button primitive 不接受 ref；把焦点落点挂在包裹元素上，再取其内部按钮。
  const cancelDisableRef = useRef<HTMLDivElement>(null)
  const bodyRef = useRef<HTMLTextAreaElement>(null)
  const subjectRef = useRef<HTMLInputElement>(null)
  const ctaTextRef = useRef<HTMLInputElement>(null)
  const ctaUrlRef = useRef<HTMLInputElement>(null)
  const fieldRefs: Record<DraftField, RefObject<HTMLInputElement | HTMLTextAreaElement | null>> = {
    subject: subjectRef,
    body: bodyRef,
    cta_text: ctaTextRef,
    cta_url: ctaUrlRef,
  }

  const key = template?.key ?? ''
  const channel = template?.channel ?? 'email'

  // 打开时回到行自身的语言；关闭后清空预览残留。
  useEffect(() => {
    if (!open || template === null) return
    setLocale(template.locale)
    setPendingLocale(null)
    setDraft(null)
    setFieldErrors({})
    setSaveError(null)
    setPreview(null)
    setPreviewError(null)
  }, [open, template])

  const detailQuery = useQuery({
    queryKey: ['notification-template-detail', key, channel, locale],
    queryFn: ({ signal }) => getTemplate(key, locale, channel, signal),
    enabled: open && template !== null,
  })

  const data = detailQuery.data
  const baseline = useMemo(() => (data === undefined ? null : baselineOf(data)), [data])

  // 权威行变化（保存后重读 / 语言切换）时丢起草稿：草稿是「相对基线」的，
  // 保留旧草稿会让新基线永远显示为 dirty，甚至把旧语言内容写回新语言。
  const baselineKey = `${data?.key ?? ''}|${data?.channel ?? ''}|${data?.locale ?? ''}|${data?.updated_at ?? ''}`
  useEffect(() => {
    setDraft(null)
    setFieldErrors({})
    setSaveError(null)
  }, [baselineKey])

  const values = draft ?? baseline
  const locked = isLockedTemplate(key)
  const dirty =
    draft !== null && baseline !== null && !sameDraft(draft, baseline)

  const issues = useMemo(() => {
    if (values === null) return { unknown: [], invalid: [] }
    const fields = [values.subject, values.body, values.cta_text, values.cta_url]
    const unknown = new Set<string>()
    const invalid = new Set<string>()
    for (const text of fields) {
      const issue = scanTemplateVariables(text, key, channel)
      for (const name of issue.unknown) unknown.add(name)
      for (const construct of issue.invalid) invalid.add(construct)
    }
    return { unknown: [...unknown], invalid: [...invalid] }
  }, [values, key, channel])

  const hasVariableIssue = issues.unknown.length > 0 || issues.invalid.length > 0

  // 危险确认的焦点落在「取消」上（WAI-ARIA alertdialog；设计 §6.9）。
  // Dialog 打开时会先聚焦面板，故延后一拍再移动焦点，避免被面板抢回。
  useEffect(() => {
    if (!pendingDisable) return
    const timer = setTimeout(
      () => cancelDisableRef.current?.querySelector('button')?.focus(),
      0,
    )
    return () => clearTimeout(timer)
  }, [pendingDisable])

  function updateField(field: keyof Draft, value: string | boolean): void {
    if (values === null) return
    const current = draft ?? baseline
    if (current === null) return
    setDraft({ ...current, [field]: value })
    setFieldErrors((errors) => {
      if (!(field in errors)) return errors
      const next = { ...errors }
      delete next[field]
      return next
    })
    setSaveError(null)
  }

  function validationErrors(candidate: Draft): FieldErrors {
    const errors: FieldErrors = {}
    if (channel === 'email') {
      if (candidate.subject.trim() === '') {
        errors.subject = t('settings.notification_templates.drawer.subject_required')
      } else if (runeLength(candidate.subject) > SUBJECT_MAX_LENGTH) {
        errors.subject = t('settings.notification_templates.drawer.too_long', {
          max: SUBJECT_MAX_LENGTH,
        })
      }
    }
    if (candidate.body.trim() === '') {
      errors.body = t('settings.notification_templates.drawer.body_required')
    } else if (runeLength(candidate.body) > BODY_MAX_LENGTH) {
      errors.body = t('settings.notification_templates.drawer.too_long', {
        max: BODY_MAX_LENGTH,
      })
    }
    if (runeLength(candidate.cta_text) > CTA_TEXT_MAX_LENGTH) {
      errors.cta_text = t('settings.notification_templates.drawer.too_long', {
        max: CTA_TEXT_MAX_LENGTH,
      })
    }
    const hasText = candidate.cta_text.trim() !== ''
    const hasUrl = candidate.cta_url.trim() !== ''
    if (hasText !== hasUrl) {
      errors.cta_url = t('settings.notification_templates.drawer.cta_pair')
    } else if (hasUrl && !isValidCtaUrl(candidate.cta_url)) {
      errors.cta_url = t('settings.notification_templates.drawer.cta_url_invalid')
    }
    return errors
  }

  function focusField(field: DraftField): void {
    fieldRefs[field].current?.focus()
  }

  async function save(): Promise<void> {
    if (!canManage || data === undefined || values === null) return
    const errors = validationErrors(values)
    if (hasVariableIssue) {
      errors.variables = t('settings.notification_templates.drawer.variables_invalid')
    }
    if (Object.keys(errors).length > 0) {
      setFieldErrors(errors)
      const first = (Object.keys(errors) as Array<keyof FieldErrors>)[0]
      if (first !== undefined && first !== 'variables' && first !== 'enabled') {
        focusField(first)
      }
      return
    }

    setSaving(true)
    setSaveError(null)
    try {
      const updated = await updateTemplate(key, {
        locale,
        // 多渠道 key 必须显式传 channel，否则服务端 400。
        channel,
        subject: channel === 'email' ? values.subject : '',
        body: values.body,
        cta_text: values.cta_text,
        cta_url: values.cta_url,
        enabled: values.enabled,
        // 乐观锁：row 尚未落库时 updated_at 为 null，服务端允许首次插入。
        expect_updated_at: data.updated_at,
      })
      // 先把权威行写回详情缓存，再通知列表失效；两者都读服务端返回值。
      queryClient.setQueryData(['notification-template-detail', key, channel, locale], updated)
      setDraft(null)
      push({ tone: 'success', message: t('settings.notification_templates.drawer.saved') })
      onSaved(updated)
    } catch (error) {
      if (error instanceof ApiError && error.kind === 'conflict') {
        setConflictOpen(true)
      } else if (error instanceof ApiError && error.kind === 'forbidden') {
        setSaveError(t('settings.notification_templates.drawer.save_forbidden'))
      } else if (error instanceof ApiError && error.kind === 'rate_limited') {
        push({ tone: 'error', message: t('settings.notification_templates.drawer.rate_limited') })
      } else if (isOfflineError(error)) {
        setSaveError(t('settings.notification_templates.drawer.offline'))
      } else {
        const field = error instanceof ApiError ? fieldForProblem(error) : null
        if (field !== null) {
          setFieldErrors({ [field]: errorMessage(error) } as FieldErrors)
          if (field === 'subject' || field === 'body' || field === 'cta_text' || field === 'cta_url') {
            focusField(field)
          }
        } else {
          setSaveError(errorMessage(error))
        }
      }
    } finally {
      setSaving(false)
    }
  }

  function requestLocaleChange(next: NotificationLocale): void {
    if (next === locale) return
    if (dirty) {
      setPendingLocale(next)
      return
    }
    setLocale(next)
  }

  function insertVariable(name: string): void {
    if (values === null) return
    const ref = fieldRefs[activeField].current
    const caret = ref?.selectionStart ?? null
    const token = `{{.${name}}}`
    const result = insertAtCursor(values[activeField], caret, token)
    updateField(activeField, result.value)
    // 让焦点回到刚编辑的字段，并把光标放到插入内容之后。
    setTimeout(() => {
      const element = fieldRefs[activeField].current
      if (element === null) return
      element.focus()
      element.setSelectionRange(result.caret, result.caret)
    }, 0)
  }

  async function loadPreview(): Promise<void> {
    setPreviewOpen(true)
    setPreview(null)
    setPreviewError(null)
    setPreviewLoading(true)
    try {
      // variables 传空对象：服务端以注册表样例值渲染（§3.9.6a）。
      // 注意端点渲染的是**已保存**内容，未保存草稿需先保存（登记在报告）。
      const rendered = await previewTemplate(key, {
        locale,
        channel,
        variables: {},
      })
      setPreview(rendered)
    } catch (error) {
      setPreviewError(errorMessage(error))
    } finally {
      setPreviewLoading(false)
    }
  }

  const fieldClass =
    'w-full rounded-lg border bg-surface-card px-3 py-2 text-sm text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

  const variables = variablesFor(key, channel)

  // 抽屉始终挂载在页面里；没有选中行时直接不渲染，避免对空 key 求值
  // （分类标签、详情查询都假定 key 非空）。
  if (template === null) return null

  return (
    <>
      <Dialog
        open={open && template !== null}
        onOpenChange={onOpenChange}
        title={key}
        variant="sheet"
        panelClassName="sm:max-w-[560px]"
        footer={
          canManage ? (
            <div className="flex w-full items-center justify-between gap-2">
              <Button variant="secondary" onClick={() => void loadPreview()}>
                {t('settings.notification_templates.drawer.preview')}
              </Button>
              <div className="flex gap-2">
                <Button variant="ghost" onClick={() => onOpenChange(false)} disabled={saving}>
                  {t('settings.notification_templates.drawer.cancel')}
                </Button>
                <Button
                  onClick={() => void save()}
                  disabled={!dirty || saving || !online || hasVariableIssue}
                >
                  {saving
                    ? t('settings.notification_templates.drawer.saving')
                    : t('settings.notification_templates.drawer.save')}
                </Button>
              </div>
            </div>
          ) : (
            <Button variant="secondary" onClick={() => onOpenChange(false)}>
              {t('settings.notification_templates.drawer.close')}
            </Button>
          )
        }
      >
        <div className="space-y-4" data-testid="notification-template-drawer">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-mono text-xs text-ink-2">
              {t('settings.notification_templates.drawer.category')}: {key}
            </span>
            <Tag variant="neutral">{t(categoryLabelKey(key))}</Tag>
            <Tag variant="neutral">{t(`settings.notification_templates.channel.${channel}`)}</Tag>
            <label className="ml-auto flex items-center gap-2 text-xs text-ink-2">
              {t('settings.notification_templates.drawer.locale')}
              <select
                aria-label={t('settings.notification_templates.drawer.locale')}
                value={locale}
                onChange={(event) => {
                  const next = event.target.value
                  if (
                    next === 'en' ||
                    next === 'zh-CN' ||
                    next === 'zh-TW' ||
                    next === 'fr' ||
                    next === 'ru' ||
                    next === 'ja' ||
                    next === 'vi'
                  ) {
                    requestLocaleChange(next)
                  }
                }}
                className="h-8 rounded-lg border border-border-default bg-surface-card px-2 text-xs text-ink-1"
              >
                {NOTIFICATION_LOCALES.map((entry) => (
                  <option key={entry} value={entry}>
                    {entry}
                  </option>
                ))}
              </select>
            </label>
          </div>

          {detailQuery.isPending ? (
            <div className="space-y-3" aria-hidden="true">
              {[0, 1, 2, 3].map((row) => (
                <div key={row} className="h-10 rounded-lg bg-surface-subtle" />
              ))}
            </div>
          ) : null}

          {detailQuery.isError ? (
            <div className="space-y-3">
              <FormAlert tone="danger">
                {t('settings.notification_templates.drawer.load_error')}{' '}
                {errorMessage(detailQuery.error)}
              </FormAlert>
              <Button variant="secondary" onClick={() => void detailQuery.refetch()}>
                {t('settings.notification_templates.drawer.retry')}
              </Button>
            </div>
          ) : null}

          {values !== null && baseline !== null ? (
            <>
              {saveError !== null ? <FormAlert tone="danger">{saveError}</FormAlert> : null}
              {!canManage ? (
                <FormAlert tone="info">
                  {t('settings.notification_templates.page.read_only')}
                </FormAlert>
              ) : null}
              {!online ? (
                <p className="text-xs text-ink-3">
                  {t('settings.notification_templates.drawer.offline')}
                </p>
              ) : null}

              <div className="flex items-center justify-between gap-6">
                <span className="text-[13px] font-medium text-ink-1">
                  {t('settings.notification_templates.drawer.enabled')}
                </span>
                {locked ? (
                  <span
                    className="text-xs text-ink-2"
                    title={t('settings.notification_templates.drawer.locked')}
                  >
                    {t('settings.notification_templates.drawer.locked_enabled')}
                  </span>
                ) : (
                  <input
                    type="checkbox"
                    role="switch"
                    aria-label={t('settings.notification_templates.drawer.enabled')}
                    aria-checked={values.enabled}
                    disabled={!canManage || saving}
                    checked={values.enabled}
                    onChange={(event) => {
                      if (!event.target.checked) {
                        setPendingDisable(true)
                        return
                      }
                      updateField('enabled', true)
                    }}
                    className="h-5 w-9 appearance-none rounded-full border border-border-default bg-surface-subtle transition-colors checked:border-brand checked:bg-brand disabled:cursor-not-allowed disabled:opacity-50"
                  />
                )}
              </div>
              {locked ? (
                <p className="text-xs text-ink-3">
                  {t('settings.notification_templates.drawer.locked')}
                </p>
              ) : null}
              {fieldErrors.enabled !== undefined ? (
                <p className="text-xs text-danger">{fieldErrors.enabled}</p>
              ) : null}

              {channel === 'email' ? (
                <label className="flex flex-col gap-1.5">
                  <span className="text-[13px] font-medium text-ink-1">
                    {t('settings.notification_templates.drawer.subject')}
                  </span>
                  <input
                    ref={subjectRef}
                    value={values.subject}
                    disabled={!canManage || saving}
                    maxLength={SUBJECT_MAX_LENGTH}
                    aria-label={t('settings.notification_templates.drawer.subject')}
                    aria-invalid={fieldErrors.subject !== undefined}
                    onFocus={() => setActiveField('subject')}
                    onChange={(event) => updateField('subject', event.target.value)}
                    className={`${fieldClass} h-10 ${fieldErrors.subject !== undefined ? 'border-danger' : 'border-border-default'}`}
                  />
                  <span className="flex justify-between text-xs text-ink-3">
                    <span>
                      {t('settings.notification_templates.drawer.subject_hint', {
                        max: SUBJECT_MAX_LENGTH,
                      })}
                    </span>
                    <span>
                      {t('settings.notification_templates.drawer.chars', {
                        count: runeLength(values.subject),
                        max: SUBJECT_MAX_LENGTH,
                      })}
                    </span>
                  </span>
                  {fieldErrors.subject !== undefined ? (
                    <span className="text-xs text-danger">{fieldErrors.subject}</span>
                  ) : null}
                </label>
              ) : null}

              <label className="flex flex-col gap-1.5">
                <span className="text-[13px] font-medium text-ink-1">
                  {t('settings.notification_templates.drawer.body')}
                </span>
                <textarea
                  ref={bodyRef}
                  value={values.body}
                  rows={6}
                  disabled={!canManage || saving}
                  aria-label={t('settings.notification_templates.drawer.body')}
                  aria-invalid={fieldErrors.body !== undefined}
                  onFocus={() => setActiveField('body')}
                  onChange={(event) => updateField('body', event.target.value)}
                  className={`${fieldClass} font-mono text-[13px] ${fieldErrors.body !== undefined ? 'border-danger' : 'border-border-default'}`}
                />
                <span className="flex justify-between text-xs text-ink-3">
                  <span>
                    {t('settings.notification_templates.drawer.body_hint', {
                      max: BODY_MAX_LENGTH,
                    })}
                  </span>
                  <span>
                    {t('settings.notification_templates.drawer.chars', {
                      count: runeLength(values.body),
                      max: BODY_MAX_LENGTH,
                    })}
                  </span>
                </span>
                {fieldErrors.body !== undefined ? (
                  <span className="text-xs text-danger">{fieldErrors.body}</span>
                ) : null}
              </label>

              <div className="grid gap-3 sm:grid-cols-2">
                <label className="flex flex-col gap-1.5">
                  <span className="text-[13px] font-medium text-ink-1">
                    {t('settings.notification_templates.drawer.cta_text')}
                  </span>
                  <input
                    ref={ctaTextRef}
                    value={values.cta_text}
                    disabled={!canManage || saving}
                    maxLength={CTA_TEXT_MAX_LENGTH}
                    aria-label={t('settings.notification_templates.drawer.cta_text')}
                    aria-invalid={fieldErrors.cta_text !== undefined}
                    onFocus={() => setActiveField('cta_text')}
                    onChange={(event) => updateField('cta_text', event.target.value)}
                    className={`${fieldClass} h-10 ${fieldErrors.cta_text !== undefined ? 'border-danger' : 'border-border-default'}`}
                  />
                  {fieldErrors.cta_text !== undefined ? (
                    <span className="text-xs text-danger">{fieldErrors.cta_text}</span>
                  ) : null}
                </label>
                <label className="flex flex-col gap-1.5">
                  <span className="text-[13px] font-medium text-ink-1">
                    {t('settings.notification_templates.drawer.cta_url')}
                  </span>
                  <input
                    ref={ctaUrlRef}
                    value={values.cta_url}
                    disabled={!canManage || saving}
                    aria-label={t('settings.notification_templates.drawer.cta_url')}
                    aria-invalid={fieldErrors.cta_url !== undefined}
                    onFocus={() => setActiveField('cta_url')}
                    onChange={(event) => updateField('cta_url', event.target.value)}
                    className={`${fieldClass} h-10 ${fieldErrors.cta_url !== undefined ? 'border-danger' : 'border-border-default'}`}
                  />
                  {fieldErrors.cta_url !== undefined ? (
                    <span className="text-xs text-danger">{fieldErrors.cta_url}</span>
                  ) : null}
                </label>
              </div>

              <section className="space-y-2" aria-labelledby={`${key}-variables`}>
                <h3 id={`${key}-variables`} className="text-[13px] font-medium text-ink-1">
                  {t('settings.notification_templates.drawer.variables')}
                </h3>
                <div className="flex flex-wrap gap-1.5">
                  {variables.map((variable) => (
                    <button
                      key={variable.name}
                      type="button"
                      disabled={!canManage || saving}
                      aria-label={t('settings.notification_templates.drawer.insert_variable', {
                        name: variable.name,
                      })}
                      onClick={() => insertVariable(variable.name)}
                      className="inline-flex items-center gap-1 rounded-md border border-border-hairline bg-surface-subtle px-2 py-0.5 font-mono text-xs text-ink-1 hover:bg-surface-hover disabled:opacity-50"
                    >
                      {variable.name}
                      <span className="text-[10px] text-ink-3">
                        {variable.required
                          ? t('settings.notification_templates.drawer.required')
                          : t('settings.notification_templates.drawer.optional')}
                      </span>
                    </button>
                  ))}
                </div>
                {issues.unknown.map((name) => (
                  <p key={name} role="alert" className="text-xs text-danger">
                    {t('settings.notification_templates.drawer.unknown_variable', { name })}
                  </p>
                ))}
                {issues.invalid.map((construct) => (
                  <p key={construct} role="alert" className="text-xs text-danger">
                    {t('settings.notification_templates.drawer.unknown_construct', {
                      construct,
                    })}
                  </p>
                ))}
                {fieldErrors.variables !== undefined ? (
                  <p className="text-xs text-danger">{fieldErrors.variables}</p>
                ) : null}
              </section>
            </>
          ) : null}
        </div>
      </Dialog>

      <PreviewDialog
        open={previewOpen}
        onOpenChange={setPreviewOpen}
        channel={channel}
        subject={preview?.subject ?? data?.subject ?? ''}
        body={preview?.body ?? data?.body ?? ''}
        loading={previewLoading}
        error={previewError}
        onRetry={() => void loadPreview()}
      />

      <Dialog
        open={conflictOpen}
        onOpenChange={(next) => {
          if (!next) setConflictOpen(false)
        }}
        title={t('settings.notification_templates.drawer.conflict_title')}
        description={t('settings.notification_templates.drawer.conflict_body')}
        role="alertdialog"
        footer={
          <Button
            onClick={() => {
              setConflictOpen(false)
              setDraft(null)
              void detailQuery.refetch()
            }}
          >
            {t('settings.notification_templates.drawer.conflict_refresh')}
          </Button>
        }
      />

      <Dialog
        open={pendingLocale !== null}
        onOpenChange={(next) => {
          if (!next) setPendingLocale(null)
        }}
        title={t('settings.notification_templates.drawer.discard_title')}
        description={t('settings.notification_templates.drawer.discard_body')}
        role="alertdialog"
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onClick={() => setPendingLocale(null)}>
              {t('settings.notification_templates.drawer.discard_cancel')}
            </Button>
            <Button
              variant="danger"
              onClick={() => {
                const next = pendingLocale
                setPendingLocale(null)
                setDraft(null)
                setFieldErrors({})
                if (next !== null) setLocale(next)
              }}
            >
              {t('settings.notification_templates.drawer.discard_confirm')}
            </Button>
          </div>
        }
      />

      <Dialog
        open={pendingDisable}
        onOpenChange={(next) => {
          if (!next) setPendingDisable(false)
        }}
        title={t('settings.notification_templates.drawer.disable_title')}
        description={t('settings.notification_templates.drawer.disable_body')}
        role="alertdialog"
        footer={
          <div className="flex justify-end gap-2">
            <div ref={cancelDisableRef}>
              <Button variant="secondary" onClick={() => setPendingDisable(false)}>
                {t('settings.notification_templates.drawer.disable_cancel')}
              </Button>
            </div>
            <Button
              variant="danger"
              onClick={() => {
                setPendingDisable(false)
                updateField('enabled', false)
              }}
            >
              {t('settings.notification_templates.drawer.disable_confirm')}
            </Button>
          </div>
        }
      />
    </>
  )
}
