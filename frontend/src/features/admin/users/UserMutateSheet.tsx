import { useEffect, useState, type FormEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { passwordPolicyProblem } from '@/lib/password-policy'
import { meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Sheet } from '@/shared/ui/base/sheet'
import { FormAlert, PasswordField, TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

import { PermissionMatrix } from './PermissionMatrix'

type AdminUser = components['schemas']['AdminUser']
type UserRole = components['schemas']['UserRole']

/**
 * Read-only starting grant the server applies on promotion
 * (`defaultAdminPermissions`, handlers_users.go:1590). The sheet mirrors it so
 * the matrix does not falsely show FULL and so an untouched promotion sends no
 * permissions PATCH.
 */
const DEFAULT_ADMIN_PERMISSIONS = ['users:read', 'logs:read'] as const

export interface UserMutateSheetProps {
  open: boolean
  mode: 'create' | 'edit'
  user?: AdminUser
  onOpenChange: (open: boolean) => void
  onSaved: () => void
}

/**
 * F-07-02/F-07-03 create and edit sheet. Both forms share the shell; the edit
 * form sends the optimistic-lock `updated_at` and can edit the RBAC matrix.
 *
 * Only fields that actually changed are sent. This is load-bearing, not
 * cosmetic: the backend revokes every session of the target whenever `role`
 * is present in the PATCH (even when the value is unchanged), rejects any
 * role change on yourself, and requires `users:manage-roles` for any role
 * field — so an unconditional `role` turned every remark edit into a forced
 * logout (review I1).
 */
export function UserMutateSheet({
  open,
  mode,
  user,
  onOpenChange,
  onSaved,
}: UserMutateSheetProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const [username, setUsername] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [role, setRole] = useState<UserRole>('user')
  const [group, setGroup] = useState('default')
  const [password, setPassword] = useState('')
  const [remark, setRemark] = useState('')
  const [permissions, setPermissions] = useState<string[] | null>(null)
  const [initialPermissions, setInitialPermissions] = useState<string | null>('null')
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [permissionError, setPermissionError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    if (!open) return
    setUsername(user?.username ?? '')
    setDisplayName(user?.display_name ?? '')
    setRole(user?.role ?? 'user')
    setGroup(user?.group ?? 'default')
    setPassword('')
    setRemark(user?.remark ?? '')
    setPermissions(user?.admin_permissions ?? null)
    setInitialPermissions(JSON.stringify(user?.admin_permissions ?? null))
    setFieldErrors({})
    setFormError(undefined)
    setPermissionError(undefined)
  }, [open, user])

  // Who am I? `MeView` has no id (R-17), so self-edit is detected by username.
  const meQuery = useQuery({ ...meQueryOptions(), enabled: open })
  const isSelf =
    mode === 'edit' && user !== undefined && meQuery.data?.username === user.username

  // MeView does not expose the caller's own permission set, so find the
  // caller's row in the same list endpoint the page already uses. The keyword
  // filter is exact-matched below; a stale/absent row leaves the capability
  // unknown and the server remains the authority.
  const actorQuery = useQuery({
    queryKey: ['admin-users', 'actor', meQuery.data?.username],
    enabled: open && meQuery.data?.username !== undefined,
    staleTime: 0,
    queryFn: async () => {
      const data = requireData(
        await apiClient.GET('/api/users', {
          params: {
            query: {
              keyword: meQuery.data?.username ?? '',
              p: 1,
              page_size: 20,
            },
          },
        }),
      )
      return (data.items ?? []).find(
        (candidate) => candidate.username === meQuery.data?.username,
      )
    },
  })
  const actorRow = actorQuery.data
  const actorPermissions = actorRow?.admin_permissions ?? undefined
  const canManageRoles =
    actorRow === undefined
      ? // Unknown while the caller row loads: do not lock the control for a
        // frame; the server rejects an unauthorized role change anyway.
        true
      : actorRow.admin_permissions === null ||
        actorRow.admin_permissions.includes('*') ||
        actorRow.admin_permissions.includes('users:manage-roles')
  const roleLocked = !canManageRoles || isSelf

  function changeRole(next: UserRole): void {
    setRole(next)
    setPermissionError(undefined)
    if (next === 'admin') {
      // Mirror the server's promotion defaults; also makes the baseline equal
      // so an untouched matrix is not PATCHed back.
      const defaults = [...DEFAULT_ADMIN_PERMISSIONS]
      setPermissions(defaults)
      setInitialPermissions(JSON.stringify(defaults))
      return
    }
    // Demotion resets permissions server-side; keep local state in sync so a
    // later re-promotion starts from the defaults, never the stale matrix.
    setPermissions([])
    setInitialPermissions(JSON.stringify([]))
  }

  async function save(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    const errors: Record<string, string> = {}
    if (mode === 'create') {
      if (username.trim() === '') errors.username = t('auth.validation.required')
      if (passwordPolicyProblem(password) !== undefined) {
        errors.password = t('auth.validation.password_length')
      }
    }
    setFieldErrors(errors)
    if (Object.keys(errors).length > 0) return

    setFormError(undefined)
    setPermissionError(undefined)
    setSubmitting(true)
    let userSaved = false
    let permissionsSaved = false
    let permissionSupersetFailure = false

    const patchPermissions = async (): Promise<void> => {
      if (user === undefined) return
      try {
        await apiClient.PATCH('/api/users/{id}/permissions', {
          params: { path: { id: user.id } },
          body: {
            admin_permissions: (permissions ??
              []) as components['schemas']['PermissionGrant'][],
          },
        })
      } catch (caught) {
        if (caught instanceof ApiError && caught.status === 403) {
          // Spec requires this on the matrix itself, not only as a toast.
          permissionSupersetFailure = true
          setPermissionError(t('admin.user_form.permission_superset'))
        }
        throw caught
      }
    }

    try {
      if (mode === 'create') {
        await apiClient.POST('/api/users', {
          body: {
            username: username.trim(),
            display_name: displayName.trim() === '' ? null : displayName.trim(),
            role,
            group,
            password,
            remark: remark.trim() === '' ? null : remark.trim(),
          },
        })
        push({ tone: 'success', message: t('admin.user_form.create_success') })
      } else if (user !== undefined) {
        // Diff against the loaded row; an absent field is "unchanged" on the
        // wire (the server treats presence as an instruction, not a value).
        const nextDisplayName = displayName.trim() === '' ? null : displayName.trim()
        const nextRemark = remark.trim() === '' ? null : remark.trim()
        const body: components['schemas']['AdminUpdateUserRequest'] = {
          updated_at: user.updated_at,
        }
        if (nextDisplayName !== (user.display_name ?? null)) {
          body.display_name = nextDisplayName
        }
        if (group !== user.group) body.group = group
        if (nextRemark !== (user.remark ?? null)) body.remark = nextRemark
        const roleChanged = role !== user.role
        if (roleChanged) body.role = role

        const userFieldsChanged =
          body.display_name !== undefined ||
          body.group !== undefined ||
          body.remark !== undefined ||
          roleChanged
        const permissionsChanged = JSON.stringify(permissions) !== initialPermissions
        // Demotion resets permissions server-side; sending the matrix after it
        // would target a non-admin and 400.
        const permissionsStep =
          permissionsChanged && (!roleChanged || role === 'admin')

        if (!userFieldsChanged && !permissionsStep) {
          // Nothing to write: reopening was a no-op, not an error.
          onOpenChange(false)
          return
        }

        if (!roleChanged) {
          // Validation-heavy step first: a superset rejection then leaves the
          // user row untouched instead of half-updating it (review I2).
          if (permissionsStep) {
            await patchPermissions()
            permissionsSaved = true
          }
          if (userFieldsChanged) {
            await apiClient.PATCH('/api/users/{id}', {
              params: { path: { id: user.id } },
              body,
            })
            userSaved = true
          }
        } else {
          // Promotion must persist the role before the permissions PATCH can
          // address an admin target.
          if (userFieldsChanged) {
            await apiClient.PATCH('/api/users/{id}', {
              params: { path: { id: user.id } },
              body,
            })
            userSaved = true
          }
          if (permissionsStep) {
            await patchPermissions()
            permissionsSaved = true
          }
        }
        push({ tone: 'success', message: t('admin.user_form.saved') })
      }
      // Both prefixes are stale after a write: the list row, and the group
      // counts/selectors (a user may have moved between groups).
      await queryClient.invalidateQueries({ queryKey: ['admin-users'] })
      await queryClient.invalidateQueries({ queryKey: ['admin-groups'] })
      onSaved()
      onOpenChange(false)
    } catch {
      if (userSaved || permissionsSaved) {
        // One of two writes landed. Refreshing the list shows the new truth;
        // the stale `updated_at` held by this sheet would 409 on retry, so the
        // copy tells the operator to reopen (review I2).
        await queryClient.invalidateQueries({ queryKey: ['admin-users'] })
        await queryClient.invalidateQueries({ queryKey: ['admin-groups'] })
        setFormError(t('admin.user_form.partially_saved'))
      } else if (!permissionSupersetFailure) {
        setFormError(t('admin.user_form.error'))
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Sheet
      open={open}
      onOpenChange={onOpenChange}
      title={
        mode === 'create'
          ? t('admin.user_form.create_title')
          : t('admin.user_form.edit_title')
      }
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            {t('common.action.cancel')}
          </Button>
          <Button type="submit" form="user-mutate-form" disabled={submitting}>
            {submitting
              ? mode === 'create'
                ? t('admin.user_form.creating')
                : t('admin.user_form.saving')
              : t('admin.user_form.save')}
          </Button>
        </>
      }
    >
      <form
        id="user-mutate-form"
        className="flex flex-col gap-4"
        onSubmit={(event) => void save(event)}
        noValidate
      >
        {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}

        <TextField
          label={t('admin.user_form.username')}
          value={username}
          disabled={mode === 'edit'}
          onChange={(event) => {
            setUsername(event.target.value)
          }}
          error={fieldErrors.username}
          required={mode === 'create'}
        />
        <TextField
          label={t('admin.user_form.display_name')}
          value={displayName}
          maxLength={32}
          onChange={(event) => {
            setDisplayName(event.target.value)
          }}
        />
        <div className="flex flex-col gap-1.5">
          <label htmlFor="user-role" className="text-meta font-medium text-ink-1">
            {t('admin.user_form.role')}
          </label>
          <select
            id="user-role"
            className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
            value={role}
            disabled={roleLocked}
            aria-describedby={roleLocked ? 'user-role-locked' : undefined}
            onChange={(event) => {
              changeRole(event.target.value as UserRole)
            }}
          >
            <option value="user">{t('admin.users.role_user')}</option>
            <option value="admin">{t('admin.users.role_admin')}</option>
          </select>
          {roleLocked ? (
            <p id="user-role-locked" className="text-meta text-ink-3">
              {isSelf
                ? t('admin.user_form.role_locked_self')
                : t('admin.user_form.role_locked_permission')}
            </p>
          ) : null}
        </div>
        <TextField
          label={t('admin.user_form.group')}
          value={group}
          onChange={(event) => {
            setGroup(event.target.value)
          }}
        />
        {mode === 'create' ? (
          <PasswordField
            label={t('admin.user_form.password')}
            showLabel={t('auth.sign_in.show_password')}
            hideLabel={t('auth.sign_in.hide_password')}
            autoComplete="new-password"
            value={password}
            onChange={(event) => {
              setPassword(event.target.value)
            }}
            error={fieldErrors.password}
            required
          />
        ) : null}
        <TextField
          label={t('admin.user_form.remark')}
          value={remark}
          maxLength={255}
          onChange={(event) => {
            setRemark(event.target.value)
          }}
        />

        {mode === 'edit' && role === 'admin' ? (
          <section className="flex flex-col gap-2 border-t border-border-hairline pt-4">
            <h3 className="text-body font-semibold text-ink-1">
              {t('admin.user_form.section_permissions')}
            </h3>
            {permissionError !== undefined ? (
              <FormAlert>{permissionError}</FormAlert>
            ) : null}
            {roleLocked ? (
              <p className="text-meta text-ink-3">
                {t('admin.user_form.permissions_locked')}
              </p>
            ) : null}
            <PermissionMatrix
              value={permissions}
              onChange={(next) => {
                setPermissions(next)
                setPermissionError(undefined)
              }}
              disabled={roleLocked}
              granted={actorPermissions ?? undefined}
            />
          </section>
        ) : null}
      </form>
    </Sheet>
  )
}
