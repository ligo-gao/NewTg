import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ApiError } from '@/api/errors'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { FormAlert, TextField } from '@/shared/ui/form'
import { useI18n } from '@/shared/lib/i18n-context'
import { useToastStore } from '@/shared/ui/toast'

import { ANNOUNCEMENT_ACTIVE_QUERY_KEY } from '@/features/announcements/query-keys'
import {
  createAnnouncement,
  getAnnouncement,
  updateAnnouncement,
  type AnnouncementCategory,
  type AnnouncementDetail,
  type AnnouncementWriteInput,
} from '@/features/announcements/api'
import { ANNOUNCEMENT_CATEGORIES } from './search'
import {
  ANNOUNCEMENT_MAX_CONTENT_BYTES,
  ANNOUNCEMENT_MAX_TITLE_RUNES,
  formatDateTime,
  fromDateTimeLocalValue,
  runeLength,
  toDateTimeLocalValue,
  utf8ByteLength,
  validateAnnouncementForm,
  type AnnouncementFormErrors,
  type AnnouncementFormValues,
} from './utils'

const SELECT_CLASSES =
  'h-10 w-full rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'
const TEXTAREA_CLASSES =
  'min-h-[200px] w-full rounded-lg border bg-surface-card px-3 py-2 font-mono text-[13px] leading-5 text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

function emptyValues(): AnnouncementFormValues {
  return {
    title: '',
    category: 'notice',
    content: '',
    // 默认立即生效；提交时空值会退化为服务端 now()。
    publishAt: toDateTimeLocalValue(new Date().toISOString()),
    expireAt: '',
    publishAtLocked: false,
  }
}

function valuesFromDetail(detail: AnnouncementDetail): AnnouncementFormValues {
  return {
    title: detail.title,
    category: detail.category,
    content: detail.content_markdown,
    publishAt: toDateTimeLocalValue(detail.publish_at),
    expireAt: toDateTimeLocalValue(detail.expire_at ?? null),
    // N-35：已发布公告的生效时间只读——服务端忽略该字段，界面也不提供
    // 一个「看起来能改、实际不会生效」的输入框。
    publishAtLocked: detail.status === 'published',
  }
}

export interface AnnouncementDrawerProps {
  open: boolean
  /** null = 新建；否则为要编辑的公告 id（列表投影不含正文，需拉详情）。 */
  announcementId: string | null
  online: boolean
  onClose: () => void
  /** 保存成功后的列表失效回调（由页面提供，避免抽屉知道缓存布局）。 */
  onSaved: () => Promise<void> | void
}

/**
 * 公告新增/编辑抽屉（UI §6.1.1，F-08-04）。
 *
 * 两个显式动作按契约字面后果呈现：
 * - 「保存草稿」= `publish_now=false`（offline 编辑会由服务端回 draft）；
 * - 「保存并发布」= `publish_now=true`，服务端单事务写+发布，失败整体回滚
 *   （新建不留草稿行、编辑不改原状态），因此错误提示必须说明「未保存」。
 *
 * 已发布公告的编辑走 N-35 的原地更新语义（publish_at/published_at 不变），
 * 此时只提供「保存更改」，不再展示一个会导致误解的「保存草稿」。
 */
export function AnnouncementDrawer({
  open,
  announcementId,
  online,
  onClose,
  onSaved,
}: AnnouncementDrawerProps) {
  const { t, locale } = useI18n()
  const push = useToastStore((state) => state.push)
  const queryClient = useQueryClient()

  const detailQuery = useQuery({
    queryKey: ['announcements', 'detail', announcementId],
    queryFn: ({ signal }) => {
      // enabled 保证 id 非空；这里显式失败关闭，避免把 undefined 当路径参数。
      if (announcementId === null) throw new Error('missing announcement id')
      return getAnnouncement(announcementId, signal)
    },
    enabled: open && announcementId !== null,
    retry: false,
    // 详情只用于初始化表单；重复聚焦不应覆盖用户正在编辑的内容。
    staleTime: Infinity,
  })

  const [values, setValues] = useState<AnnouncementFormValues>(emptyValues)
  const [clientErrors, setClientErrors] = useState<AnnouncementFormErrors>({})
  const [submitError, setSubmitError] = useState<{
    error: unknown
    publishNow: boolean
  } | null>(null)
  const [confirmOpen, setConfirmOpen] = useState(false)
  // 只在新打开/切换目标（或详情首次到达）时重置表单，避免后台重取
  // 详情时把用户输入冲掉。
  const initializedFor = useRef<string | null>(null)

  useEffect(() => {
    if (!open) {
      initializedFor.current = null
      return
    }
    if (announcementId === null) {
      if (initializedFor.current !== 'new') {
        setValues(emptyValues())
        setClientErrors({})
        setSubmitError(null)
        initializedFor.current = 'new'
      }
      return
    }
    const detail = detailQuery.data
    if (detail !== undefined && initializedFor.current !== detail.id) {
      setValues(valuesFromDetail(detail))
      setClientErrors({})
      setSubmitError(null)
      initializedFor.current = detail.id
    }
  }, [open, announcementId, detailQuery.data])

  const detail = detailQuery.data
  const isPublished = detail?.status === 'published'
  const isOfflineRow = detail?.status === 'offline'
  const detailPending = announcementId !== null && detailQuery.isPending
  const detailFailed = announcementId !== null && detailQuery.isError
  const contentBytes = utf8ByteLength(values.content)

  const saveMutation = useMutation({
    mutationFn: async (publishNow: boolean): Promise<AnnouncementDetail> => {
      const expireAt = fromDateTimeLocalValue(values.expireAt)
      const input: AnnouncementWriteInput = {
        title: values.title.trim(),
        category: values.category,
        content: values.content,
        expire_at: expireAt,
        publish_now: publishNow,
      }
      // 已发布编辑忽略 publish_at（N-35）；其余场景提交表单值，空值省略
      // 让服务端按「创建=now / 编辑=保留」处理。
      if (!values.publishAtLocked) {
        const publishAt = fromDateTimeLocalValue(values.publishAt)
        if (publishAt !== null) input.publish_at = publishAt
      }
      return announcementId === null
        ? createAnnouncement(input)
        : updateAnnouncement(announcementId, input)
    },
    retry: false,
    onSuccess: async (_detail, publishNow) => {
      const key =
        publishNow && !isPublished
          ? 'admin.announcements.toast.published'
          : announcementId === null
            ? 'admin.announcements.toast.saved'
            : 'admin.announcements.toast.updated'
      push({ tone: 'success', message: t(key) })
      await onSaved()
      // 用户侧弹层与列表共享同一份服务端事实；发布/下线/删除后必须让它
      // 重新拉取，否则新会话之前页面里的弹层队列仍是旧快照。
      await queryClient.invalidateQueries({ queryKey: ANNOUNCEMENT_ACTIVE_QUERY_KEY })
      onClose()
    },
    onError: (error, publishNow) => {
      // publish_now 是单事务原子路径：422 意味着整体回滚，界面必须说清
      // 「什么都没保存」，而不是笼统的「保存失败」。
      setSubmitError({ error, publishNow })
    },
  })

  function runValidation(): boolean {
    const nextErrors = validateAnnouncementForm(
      values,
      isPublished ? (detail?.publish_at ?? null) : null,
      {
        titleRequired: t('admin.announcements.drawer.title_required'),
        titleTooLong: t('admin.announcements.drawer.title_too_long', {
          max: ANNOUNCEMENT_MAX_TITLE_RUNES,
        }),
        contentRequired: t('admin.announcements.drawer.content_required'),
        contentTooLong: t('admin.announcements.drawer.content_too_long', {
          max: ANNOUNCEMENT_MAX_CONTENT_BYTES,
        }),
        publishAtInvalid: t('admin.announcements.drawer.publish_at_invalid'),
        expireAtInvalid: t('admin.announcements.drawer.expire_at_invalid'),
        expireAtNotAfter: t('admin.announcements.drawer.expire_at_not_after'),
      },
    )
    setClientErrors(nextErrors)
    return Object.keys(nextErrors).length === 0
  }

  function save(publishNow: boolean): void {
    setSubmitError(null)
    if (!runValidation()) return
    if (publishNow && !isPublished) {
      // 发布是用户可见的立即副作用：先确认再提交（role=alertdialog）。
      setConfirmOpen(true)
      return
    }
    saveMutation.mutate(publishNow)
  }

  const saving = saveMutation.isPending
  const disabled = !online || saving || detailPending || detailFailed

  const serverFields =
    submitError?.error instanceof ApiError ? submitError.error.fields : undefined

  function fieldError(
    field: keyof AnnouncementFormErrors,
  ): string | undefined {
    if (clientErrors[field] !== undefined) return clientErrors[field]
    if (serverFields?.includes(field) === true) {
      return t(`admin.announcements.drawer.field_invalid.${field}`)
    }
    return undefined
  }

  function submitMessage(): string | null {
    if (submitError === null) return null
    const { error, publishNow } = submitError
    if (error instanceof ApiError) {
      if (error.kind === 'offline') {
        return t('admin.announcements.drawer.error_offline')
      }
      if (error.kind === 'rate_limited') {
        return error.retry_after !== undefined
          ? t('admin.announcements.rate_limited_retry', {
              seconds: error.retry_after,
            })
          : t('admin.announcements.rate_limited')
      }
      if (error.kind === 'validation' && publishNow) {
        return t('admin.announcements.drawer.publish_failed_rollback')
      }
    }
    return t('admin.announcements.drawer.save_failed')
  }

  const title = announcementId === null
    ? t('admin.announcements.drawer.create_title')
    : t('admin.announcements.drawer.edit_title')

  const footer = (
    <div className="flex w-full items-center justify-between gap-2">
      <Button variant="ghost" onClick={onClose}>
        {t('common.action.cancel')}
      </Button>
      <div className="flex items-center gap-2">
        {isPublished ? (
          <Button
            disabled={disabled}
            title={!online ? t('admin.announcements.offline_write_disabled') : undefined}
            onClick={() => {
              save(false)
            }}
          >
            {saving
              ? t('admin.announcements.drawer.saving')
              : t('admin.announcements.drawer.save_changes')}
          </Button>
        ) : (
          <>
            <Button
              variant="ghost"
              disabled={disabled}
              title={!online ? t('admin.announcements.offline_write_disabled') : undefined}
              onClick={() => {
                save(false)
              }}
            >
              {t('admin.announcements.drawer.save_draft')}
            </Button>
            <Button
              disabled={disabled}
              title={!online ? t('admin.announcements.offline_write_disabled') : undefined}
              onClick={() => {
                save(true)
              }}
            >
              {saving
                ? t('admin.announcements.drawer.saving')
                : t('admin.announcements.drawer.save_and_publish')}
            </Button>
          </>
        )}
      </div>
    </div>
  )

  return (
    <>
      <Dialog
        open={open}
        onOpenChange={(next) => {
          if (!next) onClose()
        }}
        variant="sheet"
        panelClassName="!max-w-[480px]"
        title={title}
        footer={footer}
      >
        <div className="flex flex-col gap-4">
          {!online ? (
            <FormAlert tone="warning">
              {t('admin.announcements.offline_write_disabled')}
            </FormAlert>
          ) : null}

          {detailPending ? (
            <div data-testid="announcement-detail-skeleton" aria-hidden="true" className="space-y-2">
              <div className="h-4 w-2/3 rounded bg-surface-subtle" />
              <div className="h-10 w-full rounded-lg bg-surface-subtle" />
              <div className="h-4 w-1/2 rounded bg-surface-subtle" />
              <div className="h-24 w-full rounded-lg bg-surface-subtle" />
            </div>
          ) : null}

          {detailFailed ? (
            <div className="space-y-3">
              <FormAlert tone="danger">
                {t('admin.announcements.drawer.load_error')}
              </FormAlert>
              <Button
                variant="secondary"
                onClick={() => {
                  void detailQuery.refetch()
                }}
              >
                {t('common.action.retry')}
              </Button>
            </div>
          ) : null}

          {!detailPending && !detailFailed ? (
            <>
              {isPublished ? (
                <FormAlert tone="info">
                  {t('admin.announcements.drawer.published_hint')}
                </FormAlert>
              ) : null}
              {isOfflineRow ? (
                <FormAlert tone="info">
                  {t('admin.announcements.drawer.offline_hint')}
                </FormAlert>
              ) : null}

              {submitMessage() !== null ? (
                <FormAlert tone={submitError?.error instanceof ApiError && submitError.error.kind === 'rate_limited' ? 'warning' : 'danger'}>
                  {submitMessage()}
                </FormAlert>
              ) : null}

              <TextField
                label={t('admin.announcements.drawer.title_label')}
                value={values.title}
                hint={t('admin.announcements.drawer.title_hint', {
                  max: ANNOUNCEMENT_MAX_TITLE_RUNES,
                })}
                error={fieldError('title')}
                onChange={(event) => {
                  setValues((current) => ({ ...current, title: event.target.value }))
                }}
              />

              <div className="flex flex-col gap-1.5">
                <label
                  htmlFor="announcement-category"
                  className="text-[13px] font-medium text-ink-1"
                >
                  {t('admin.announcements.drawer.category_label')}
                </label>
                <select
                  id="announcement-category"
                  className={SELECT_CLASSES}
                  value={values.category}
                  disabled={saving}
                  onChange={(event) => {
                    setValues((current) => ({
                      ...current,
                      category: event.target.value as AnnouncementCategory,
                    }))
                  }}
                >
                  {ANNOUNCEMENT_CATEGORIES.map((category) => (
                    <option key={category} value={category}>
                      {t(`admin.announcements.category.${category}`)}
                    </option>
                  ))}
                </select>
              </div>

              <div className="flex flex-col gap-1.5">
                <label
                  htmlFor="announcement-content"
                  className="text-[13px] font-medium text-ink-1"
                >
                  {t('admin.announcements.drawer.content_label')}
                </label>
                <textarea
                  id="announcement-content"
                  className={[
                    TEXTAREA_CLASSES,
                    fieldError('content') !== undefined
                      ? 'border-danger'
                      : 'border-border-default',
                  ].join(' ')}
                  aria-invalid={fieldError('content') !== undefined || undefined}
                  value={values.content}
                  disabled={saving}
                  onChange={(event) => {
                    setValues((current) => ({
                      ...current,
                      content: event.target.value,
                    }))
                  }}
                />
                <div className="flex items-center justify-between gap-2 text-xs text-ink-3">
                  <span>{t('admin.announcements.drawer.content_hint')}</span>
                  <span
                    data-testid="announcement-content-counter"
                    className={
                      contentBytes > ANNOUNCEMENT_MAX_CONTENT_BYTES
                        ? 'font-medium text-danger'
                        : undefined
                    }
                  >
                    {t('admin.announcements.drawer.content_counter', {
                      used: contentBytes,
                      max: ANNOUNCEMENT_MAX_CONTENT_BYTES,
                      chars: runeLength(values.content),
                    })}
                  </span>
                </div>
                {fieldError('content') !== undefined ? (
                  <p className="text-xs text-danger">{fieldError('content')}</p>
                ) : null}
              </div>

              {values.publishAtLocked ? (
                <div className="flex flex-col gap-1.5">
                  <span className="text-[13px] font-medium text-ink-1">
                    {t('admin.announcements.drawer.publish_at_label')}
                  </span>
                  <p
                    data-testid="announcement-publish-at-locked"
                    className="h-10 rounded-lg border border-border-hairline bg-surface-subtle px-3 py-2 text-sm text-ink-2"
                  >
                    {formatDateTime(detail?.publish_at, locale)}
                  </p>
                  <p className="text-xs text-ink-3">
                    {t('admin.announcements.drawer.publish_at_readonly')}
                  </p>
                </div>
              ) : (
                <TextField
                  label={t('admin.announcements.drawer.publish_at_label')}
                  type="datetime-local"
                  value={values.publishAt}
                  error={fieldError('publish_at')}
                  onChange={(event) => {
                    setValues((current) => ({
                      ...current,
                      publishAt: event.target.value,
                    }))
                  }}
                />
              )}

              <TextField
                label={t('admin.announcements.drawer.expire_at_label')}
                type="datetime-local"
                value={values.expireAt}
                hint={t('admin.announcements.drawer.expire_at_hint')}
                error={fieldError('expire_at')}
                onChange={(event) => {
                  setValues((current) => ({
                    ...current,
                    expireAt: event.target.value,
                  }))
                }}
              />
            </>
          ) : null}
        </div>
      </Dialog>

      <Dialog
        open={confirmOpen}
        onOpenChange={setConfirmOpen}
        role="alertdialog"
        title={t('admin.announcements.confirm.publish_title')}
        description={t('admin.announcements.confirm.publish_body')}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onClick={() => setConfirmOpen(false)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              onClick={() => {
                setConfirmOpen(false)
                setSubmitError(null)
                saveMutation.mutate(true)
              }}
            >
              {t('admin.announcements.confirm.publish_confirm')}
            </Button>
          </div>
        }
      />
    </>
  )
}
