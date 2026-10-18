import { useEffect, useState, type FormEvent } from 'react'
import { useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import type { components } from '@/api/schema'
import { passwordPolicyProblem } from '@/lib/password-policy'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Sheet } from '@/shared/ui/base/sheet'
import { FormAlert, PasswordField, TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

import { PermissionMatrix } from './PermissionMatrix'

type AdminUser = components['schemas']['AdminUser']
type UserRole = components['schemas']['UserRole']

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
  }, [open, user])

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
    setSubmitting(true)
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
        await apiClient.PATCH('/api/users/{id}', {
          params: { path: { id: user.id } },
          body: {
            display_name: displayName.trim() === '' ? null : displayName.trim(),
            group,
            role,
            remark: remark.trim() === '' ? null : remark.trim(),
            updated_at: user.updated_at,
          },
        })
        if (JSON.stringify(permissions) !== initialPermissions) {
          await apiClient.PATCH('/api/users/{id}/permissions', {
            params: { path: { id: user.id } },
            body: {
              admin_permissions: (permissions ??
                []) as components['schemas']['PermissionGrant'][],
            },
          })
        }
        push({ tone: 'success', message: t('admin.user_form.saved') })
      }
      // The list row (and any count/filter result) is stale after create/edit.
      await queryClient.invalidateQueries({ queryKey: ['admin-users'] })
      onSaved()
      onOpenChange(false)
    } catch {
      setFormError(t('admin.user_form.error'))
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
            {submitting ? t('admin.user_form.creating') : t('admin.user_form.save')}
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
          <label htmlFor="user-role" className="text-[13px] font-medium text-ink-1">
            {t('admin.user_form.role')}
          </label>
          <select
            id="user-role"
            className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            value={role}
            onChange={(event) => {
              setRole(event.target.value as UserRole)
            }}
          >
            <option value="user">{t('admin.users.role_user')}</option>
            <option value="admin">{t('admin.users.role_admin')}</option>
          </select>
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

        {role === 'admin' ? (
          <section className="flex flex-col gap-2 border-t border-border-hairline pt-4">
            <h3 className="text-sm font-semibold text-ink-1">
              {t('admin.user_form.section_permissions')}
            </h3>
            <PermissionMatrix value={permissions} onChange={setPermissions} />
          </section>
        ) : null}
      </form>
    </Sheet>
  )
}
