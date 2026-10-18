import { useRef } from 'react'

import type { components } from '@/api/schema'
import { useI18n } from '@/shared/lib/i18n-context'

type PermissionKey = components['schemas']['PermissionKey']

interface PermissionEntry {
  key: PermissionKey
  labelKey: string
}

interface PermissionGroup {
  key: string
  labelKey: string
  permissions: PermissionEntry[]
}

/**
 * Frozen 20-key directory (modules-07 §5.9, contract `PermissionKey`).
 * Group order is Users / Groups / Channels / Pricing / Redemption / Orders /
 * Logs / Settings — exactly the contract order; never re-sort by label.
 */
export const PERMISSION_GROUPS: readonly PermissionGroup[] = [
  {
    key: 'users',
    labelKey: 'admin.permissions.group_users',
    permissions: [
      { key: 'users:read', labelKey: 'admin.permissions.users_read' },
      { key: 'users:create', labelKey: 'admin.permissions.users_create' },
      { key: 'users:update', labelKey: 'admin.permissions.users_update' },
      { key: 'users:disable', labelKey: 'admin.permissions.users_disable' },
      { key: 'users:delete', labelKey: 'admin.permissions.users_delete' },
      { key: 'users:adjust-quota', labelKey: 'admin.permissions.users_adjust-quota' },
      { key: 'users:manage-roles', labelKey: 'admin.permissions.users_manage-roles' },
      {
        key: 'users:reset-security',
        labelKey: 'admin.permissions.users_reset-security',
      },
    ],
  },
  {
    key: 'groups',
    labelKey: 'admin.permissions.group_groups',
    permissions: [{ key: 'groups:manage', labelKey: 'admin.permissions.groups_manage' }],
  },
  {
    key: 'channels',
    labelKey: 'admin.permissions.group_channels',
    permissions: [
      { key: 'channels:read', labelKey: 'admin.permissions.channels_read' },
      { key: 'channels:manage', labelKey: 'admin.permissions.channels_manage' },
      {
        key: 'channels:manage_sensitive',
        labelKey: 'admin.permissions.channels_manage_sensitive',
      },
    ],
  },
  {
    key: 'pricing',
    labelKey: 'admin.permissions.group_pricing',
    permissions: [
      { key: 'pricing:read', labelKey: 'admin.permissions.pricing_read' },
      { key: 'pricing:manage', labelKey: 'admin.permissions.pricing_manage' },
    ],
  },
  {
    key: 'redemption',
    labelKey: 'admin.permissions.group_redemption',
    permissions: [
      { key: 'redemption:manage', labelKey: 'admin.permissions.redemption_manage' },
    ],
  },
  {
    key: 'orders',
    labelKey: 'admin.permissions.group_orders',
    permissions: [
      { key: 'orders:read', labelKey: 'admin.permissions.orders_read' },
      { key: 'orders:manage', labelKey: 'admin.permissions.orders_manage' },
    ],
  },
  {
    key: 'logs',
    labelKey: 'admin.permissions.group_logs',
    permissions: [
      { key: 'logs:read', labelKey: 'admin.permissions.logs_read' },
      { key: 'log.prompt.view', labelKey: 'admin.permissions.log_prompt_view' },
    ],
  },
  {
    key: 'settings',
    labelKey: 'admin.permissions.group_settings',
    permissions: [
      { key: 'settings:manage', labelKey: 'admin.permissions.settings_manage' },
    ],
  },
]

export const ALL_PERMISSION_KEYS: readonly PermissionKey[] = PERMISSION_GROUPS.flatMap(
  (group) => group.permissions.map((permission) => permission.key),
)

export interface PermissionMatrixProps {
  /** `null` means FULL (database NULL); `["*"]` is the explicit full grant. */
  value: string[] | null
  onChange: (value: string[]) => void
  disabled?: boolean
  /**
   * Permissions held by the acting administrator. When provided, keys outside
   * this set are disabled (the server still enforces the superset check).
   */
  granted?: string[] | null
}

function isFull(value: PermissionMatrixProps['value']): boolean {
  return value === null || value.includes('*')
}

/**
 * RBAC checkbox matrix (modules-07 §5). Checking "*" disables the 20 concrete
 * keys; unchecking restores the selection that was in place before the toggle
 * (session memory), never a silently emptied matrix.
 */
export function PermissionMatrix({
  value,
  onChange,
  disabled = false,
  granted,
}: PermissionMatrixProps) {
  const { t } = useI18n()
  const remembered = useRef<string[]>([])
  const full = isFull(value)
  const specific = value?.filter((key) => key !== '*') ?? []

  const grantAll =
    granted === undefined || granted === null || granted.includes('*')

  function toggleFull(checked: boolean): void {
    if (checked) {
      remembered.current = ALL_PERMISSION_KEYS.filter((key) =>
        specific.includes(key),
      )
      onChange(['*'])
      return
    }
    onChange([...remembered.current])
  }

  function togglePermission(key: PermissionKey, checked: boolean): void {
    const next = checked
      ? [...specific, key]
      : specific.filter((current) => current !== key)
    // Canonical order keeps saved arrays stable and diffable.
    onChange(ALL_PERMISSION_KEYS.filter((candidate) => next.includes(candidate)))
  }

  return (
    <div className="flex flex-col gap-3">
      <label className="flex items-center gap-2 rounded-lg bg-surface-subtle px-3 py-2 text-sm text-ink-1">
        <input
          type="checkbox"
          className="size-4 accent-brand"
          checked={full}
          disabled={disabled}
          onChange={(event) => toggleFull(event.target.checked)}
        />
        <span className="font-medium">{t('admin.permissions.full_access')}</span>
        <code className="font-mono text-xs text-ink-2">*</code>
      </label>

      <div className="flex flex-col gap-3">
        {PERMISSION_GROUPS.map((group) => (
          <fieldset key={group.key} className="rounded-lg border border-border-hairline p-3">
            <legend className="px-1 text-[13px] font-medium text-ink-1">
              {t(group.labelKey)}
            </legend>
            <div className="grid grid-cols-1 gap-x-4 gap-y-2 sm:grid-cols-2">
              {group.permissions.map((permission) => {
                const notGranted =
                  granted !== undefined && granted !== null && !grantAll
                    ? !granted.includes(permission.key)
                    : false
                const checked = !full && specific.includes(permission.key)
                return (
                  <label
                    key={permission.key}
                    className="flex items-start gap-2 text-[13px] text-ink-2"
                    title={
                      notGranted ? t('admin.permissions.not_held') : undefined
                    }
                  >
                    <input
                      type="checkbox"
                      className="mt-0.5 size-4 accent-brand"
                      checked={checked}
                      disabled={disabled || full || notGranted}
                      onChange={(event) =>
                        togglePermission(permission.key, event.target.checked)
                      }
                    />
                    <span>
                      <span className="text-ink-1">{t(permission.labelKey)}</span>{' '}
                      <code className="font-mono text-xs text-ink-2">
                        {permission.key}
                      </code>
                      {permission.key === 'settings:manage' ? (
                        <span className="ml-1 text-xs font-medium text-warning">
                          {t('admin.permissions.sensitive')}
                        </span>
                      ) : null}
                    </span>
                  </label>
                )
              })}
            </div>
          </fieldset>
        ))}
      </div>

      <p className="text-xs text-ink-3">{t('admin.permissions.hint')}</p>
    </div>
  )
}
