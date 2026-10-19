import type { ReactNode } from 'react'

import { formatRelativeTime } from '@/shared/lib/datetime'
import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { StatusBadge } from '@/shared/ui/status-badge'
import { Tag } from '@/shared/ui/tag'

import type { AuditCategory, AuditLogItem } from './search'
import { auditActionLabel } from './actions'

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null
    ? (value as Record<string, unknown>)
    : undefined
}

function readString(
  source: Record<string, unknown> | undefined,
  keys: readonly string[],
): string | undefined {
  for (const key of keys) {
    const value = source?.[key]
    if (typeof value === 'string' && value !== '') return value
    if (typeof value === 'number' && Number.isFinite(value)) {
      return String(value)
    }
  }
  return undefined
}

/**
 * Whitelist for `before`/`after` snapshots (sub-feature §7.4). The backend is
 * expected to have sanitized these already, but the UI applies its own
 * allowlist so a regression can never print a key, TOTP secret or backup code.
 */
const SAFE_DIFF_KEYS = new Set([
  'name',
  'status',
  'group',
  'display_name',
  'email',
  'remark',
  'model_limits',
  'allow_ips',
  'unlimited_quota',
  'remain_quota',
  'quota',
  'expired_time',
  'enabled',
  'two_factor_enabled',
])

const SECRET_KEY_PATTERN =
  /api_?key|secret|password|passwd|totp|backup|cookie|authorization|credential/i

function renderValue(value: unknown): string {
  if (value === null || value === undefined) return '—'
  if (typeof value === 'string') return value
  if (typeof value === 'number' || typeof value === 'boolean') {
    return String(value)
  }
  if (Array.isArray(value)) return value.map((item) => renderValue(item)).join(', ')
  return JSON.stringify(value)
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="contents">
      <dt className="text-[13px] text-ink-3">{label}</dt>
      <dd className="min-w-0 break-words text-sm text-ink-1">{children}</dd>
    </div>
  )
}

function CategoryFields({
  category,
  detail,
}: {
  category: AuditCategory
  detail: Record<string, unknown> | undefined
}) {
  const { t } = useI18n()
  const rows: { key: string; label: string; value: string | undefined }[] = []

  if (category === 'login') {
    rows.push({
      key: 'login_method',
      label: t('logs.audit.detail.login_method'),
      value: readString(detail, ['login_method', 'method', 'provider']),
    })
    rows.push({
      key: 'failure_reason',
      label: t('logs.audit.detail.failure_reason'),
      value: readString(detail, ['failure_reason', 'reason']),
    })
  } else if (category === 'security') {
    rows.push({
      key: 'target',
      label: t('logs.audit.detail.target'),
      value: readString(detail, ['target', 'resource_type']),
    })
    rows.push({
      key: 'action',
      label: t('logs.audit.detail.action'),
      value: auditActionLabel(t, readString(detail, ['action'])),
    })
  } else if (category === 'operation') {
    rows.push({
      key: 'resource_type',
      label: t('logs.audit.detail.resource_type'),
      value: readString(detail, ['resource_type']),
    })
    rows.push({
      key: 'resource_id',
      label: t('logs.audit.detail.resource_id'),
      value: readString(detail, ['resource_id']),
    })
    rows.push({
      key: 'action',
      label: t('logs.audit.detail.action'),
      value: auditActionLabel(t, readString(detail, ['action'])),
    })
  } else {
    rows.push({
      key: 'token_ref',
      label: t('logs.audit.detail.token_ref'),
      value: readString(detail, ['token_ref', 'token_id', 'token_name']),
    })
    rows.push({
      key: 'action',
      label: t('logs.audit.detail.action'),
      value: auditActionLabel(t, readString(detail, ['action'])),
    })
    rows.push({
      key: 'source_ip',
      label: t('logs.audit.detail.source_ip'),
      value: readString(detail, ['source_ip', 'ip']),
    })
  }

  const visible = rows.filter((row) => row.value !== undefined)
  if (visible.length === 0) return null

  return (
    <dl className="grid grid-cols-[150px_1fr] gap-x-4 gap-y-2">
      {visible.map((row) => (
        <Field key={row.key} label={row.label}>
          <span className="font-mono text-[13px]">{row.value}</span>
        </Field>
      ))}
    </dl>
  )
}

function BeforeAfterDiff({
  before,
  after,
}: {
  before: Record<string, unknown> | undefined
  after: Record<string, unknown> | undefined
}) {
  const { t } = useI18n()
  const keys = Array.from(
    new Set([...Object.keys(before ?? {}), ...Object.keys(after ?? {})]),
  ).filter(
    (key) => SAFE_DIFF_KEYS.has(key) && !SECRET_KEY_PATTERN.test(key),
  )

  if (keys.length === 0) return null

  return (
    <div className="mt-4">
      <p className="mb-2 text-[13px] font-medium text-ink-1">
        {t('logs.audit.detail.changed')}
      </p>
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
            <th scope="col" className="py-1.5 pr-3 font-medium">
              {t('logs.audit.detail.field')}
            </th>
            <th scope="col" className="py-1.5 pr-3 font-medium">
              {t('logs.audit.detail.before')}
            </th>
            <th scope="col" className="py-1.5 font-medium">
              {t('logs.audit.detail.after')}
            </th>
          </tr>
        </thead>
        <tbody>
          {keys.map((key) => (
            <tr key={key} className="border-b border-border-hairline last:border-0">
              <th scope="row" className="py-1.5 pr-3 text-left font-mono text-[12px] font-normal text-ink-3">
                {key}
              </th>
              <td className="py-1.5 pr-3 font-mono text-[13px] text-ink-2">
                <del className="no-underline">{renderValue(before?.[key])}</del>
              </td>
              <td className="py-1.5 font-mono text-[13px] text-ink-1">
                <ins className="no-underline">{renderValue(after?.[key])}</ins>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

export interface AuditDetailDialogProps {
  item: AuditLogItem | null
  onOpenChange: (open: boolean) => void
  /** Whether the username may be shown (admin + logs:read). */
  showUsername: boolean
}

/**
 * F-05-05 detail dialog. General fields are always safe; category fields are
 * pulled from an allowlist and never include secrets, TOTP material or backup
 * codes. `before`/`after` render as a diff for the whitelisted fields only.
 */
export function AuditDetailDialog({
  item,
  onOpenChange,
  showUsername,
}: AuditDetailDialogProps) {
  const { t, locale } = useI18n()
  const detail = asRecord(item?.detail)
  const before = asRecord(item?.before)
  const after = asRecord(item?.after)

  return (
    <Dialog
      open={item !== null}
      onOpenChange={onOpenChange}
      title={t('logs.audit.detail.title')}
      footer={
        <Button variant="secondary" onClick={() => onOpenChange(false)}>
          {t('logs.audit.detail.close')}
        </Button>
      }
    >
      {item !== null ? (
        <div className="flex flex-col gap-4">
          <section>
            <h3 className="mb-2 text-[15px] font-semibold text-ink-1">
              {t('logs.audit.detail.general')}
            </h3>
            <dl className="grid grid-cols-[150px_1fr] gap-x-4 gap-y-2">
              <Field label={t('logs.audit.detail.time')}>
                <span title={new Date(item.created_at).toISOString()}>
                  {new Date(item.created_at).toLocaleString(locale)} (
                  {formatRelativeTime(item.created_at, locale)})
                </span>
              </Field>
              {showUsername && item.username !== undefined ? (
                <Field label={t('logs.audit.detail.username')}>
                  {item.username}
                </Field>
              ) : null}
              <Field label={t('logs.audit.detail.ip')}>
                <span className="font-mono text-[13px]">{item.ip ?? '—'}</span>
              </Field>
              <Field label={t('logs.audit.detail.user_agent')}>
                {item.user_agent ?? '—'}
              </Field>
              <Field label={t('logs.audit.detail.method')}>
                <span className="font-mono text-[13px]">{item.method ?? '—'}</span>
              </Field>
              <Field label={t('logs.audit.detail.route')}>
                <span className="font-mono text-[13px]">{item.route ?? '—'}</span>
              </Field>
              <Field label={t('logs.audit.detail.status_code')}>
                {item.status_code ?? '—'}
              </Field>
              <Field label={t('logs.audit.detail.result')}>
                <StatusBadge
                  variant={item.result === 'success' ? 'success' : 'danger'}
                >
                  {t(`logs.audit.result.${item.result}`)}
                </StatusBadge>
              </Field>
            </dl>
          </section>

          <section className="border-t border-border-hairline pt-3">
            <h3 className="mb-2 text-[15px] font-semibold text-ink-1">
              {t('logs.audit.detail.category_fields')}
            </h3>
            <Tag variant="neutral">
              {t(`logs.audit.category.${item.category}`)}
            </Tag>
            <div className="mt-3">
              <CategoryFields category={item.category} detail={detail} />
            </div>
            <BeforeAfterDiff before={before} after={after} />
          </section>
        </div>
      ) : null}
    </Dialog>
  )
}
