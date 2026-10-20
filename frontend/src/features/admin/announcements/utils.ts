/**
 * 公告页的小工具。纯函数，便于单测与抽屉/表格复用。
 */
import type {
  AnnouncementCategory,
  AnnouncementDisplayStatus,
} from '@/features/announcements/api'

/** 正文上限：契约冻结为 16384 **UTF-8 字节**（不是字符数）。 */
export const ANNOUNCEMENT_MAX_CONTENT_BYTES = 16384
/** 标题上限：与服务端 `utf8.RuneCountInString` 同口径。 */
export const ANNOUNCEMENT_MAX_TITLE_RUNES = 80

const encoder = new TextEncoder()

/** UTF-8 字节数；多字节字符（中文 3 字节、emoji 4 字节）按实际编码计。 */
export function utf8ByteLength(value: string): number {
  return encoder.encode(value).length
}

/** Unicode 码点计数，与服务端标题长度的 rune 口径一致。 */
export function runeLength(value: string): number {
  return [...value].length
}

/**
 * ISO 时间 → `<input type="datetime-local">` 的本地时间串。
 *
 * datetime-local 没有时区信息，`new Date(...)` 会按浏览器本地时区解释；
 * 展示与回写都走同一条本地时间路径，避免把 UTC 字符串直接截断导致偏移。
 */
export function toDateTimeLocalValue(iso: string | null | undefined): string {
  if (iso === null || iso === undefined || iso === '') return ''
  const timestamp = Date.parse(iso)
  if (Number.isNaN(timestamp)) return ''
  const date = new Date(timestamp)
  const pad = (value: number): string => String(value).padStart(2, '0')
  return [
    `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`,
    `${pad(date.getHours())}:${pad(date.getMinutes())}`,
  ].join('T')
}

/** datetime-local 的本地时间串 → RFC 3339（UTC）；空值/非法值返回 null。 */
export function fromDateTimeLocalValue(value: string): string | null {
  if (value === '') return null
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp)) return null
  return new Date(timestamp).toISOString()
}

/** 表格/抽屉里的绝对时间；非法或缺失显示破折号。 */
export function formatDateTime(
  iso: string | null | undefined,
  locale: string,
): string {
  if (iso === null || iso === undefined || iso === '') return '—'
  const timestamp = Date.parse(iso)
  if (Number.isNaN(timestamp)) return '—'
  return new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(timestamp)
}

export interface AnnouncementFormValues {
  title: string
  category: AnnouncementCategory
  content: string
  /** 本地时间串（datetime-local）。 */
  publishAt: string
  /** 本地时间串；空 = 永久有效。 */
  expireAt: string
  /** 已发布公告的生效时间由服务端保留（N-35），表单只读。 */
  publishAtLocked: boolean
}

export type AnnouncementFieldError = 'title' | 'content' | 'publish_at' | 'expire_at'
export type AnnouncementFormErrors = Partial<
  Record<AnnouncementFieldError, string>
>

/**
 * 抽屉表单校验。文案键由调用方传入，纯函数只决定「哪个字段错了」。
 *
 * 生效/过期都从本地时间串解析后再比较，与服务端
 * `expire_at.After(publish_at)` 同口径；生效时间只读时用服务端值作为比较基准。
 */
export function validateAnnouncementForm(
  values: AnnouncementFormValues,
  comparePublishAtIso: string | null,
  messages: {
    titleRequired: string
    titleTooLong: string
    contentRequired: string
    contentTooLong: string
    publishAtInvalid: string
    expireAtInvalid: string
    expireAtNotAfter: string
  },
): AnnouncementFormErrors {
  const errors: AnnouncementFormErrors = {}
  const title = values.title.trim()
  if (title === '') {
    errors.title = messages.titleRequired
  } else if (runeLength(title) > ANNOUNCEMENT_MAX_TITLE_RUNES) {
    errors.title = messages.titleTooLong
  }

  if (values.content === '') {
    errors.content = messages.contentRequired
  } else if (utf8ByteLength(values.content) > ANNOUNCEMENT_MAX_CONTENT_BYTES) {
    errors.content = messages.contentTooLong
  }

  const publishIso = values.publishAtLocked
    ? comparePublishAtIso
    : fromDateTimeLocalValue(values.publishAt)
  if (!values.publishAtLocked && values.publishAt !== '' && publishIso === null) {
    errors.publish_at = messages.publishAtInvalid
  }

  if (values.expireAt !== '') {
    const expireIso = fromDateTimeLocalValue(values.expireAt)
    if (expireIso === null) {
      errors.expire_at = messages.expireAtInvalid
    } else if (publishIso !== null && Date.parse(expireIso) <= Date.parse(publishIso)) {
      // 服务端要求 expire_at **严格晚于** publish_at。
      errors.expire_at = messages.expireAtNotAfter
    }
  }

  return errors
}

const STATUS_BADGE_VARIANTS = {
  draft: 'neutral-outline',
  published: 'success',
  expired: 'warning',
  offline: 'neutral',
} as const

export function statusBadgeVariant(status: AnnouncementDisplayStatus) {
  return STATUS_BADGE_VARIANTS[status]
}
