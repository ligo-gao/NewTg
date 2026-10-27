import { useEffect, useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { formatCredits } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { Sheet } from '@/shared/ui/base/sheet'
import { FormAlert, TextField } from '@/shared/ui/form'
import { Tag } from '@/shared/ui/tag'
import { useToastStore } from '@/shared/ui/toast'

import { DiscountRuleTable } from './DiscountRuleTable'

type AdminGroup = components['schemas']['AdminGroup']

const GROUPS_KEY = ['admin-groups'] as const

// Frozen by the group contract: ^[a-z0-9_-]{2,32}$.
const GROUP_NAME_PATTERN = /^[a-z0-9_-]{2,32}$/

/** F-07-10 user groups list. Delete is guarded by the server's reference 409. */
export function GroupListPage() {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const [page, setPage] = useState(1)
  const pageSize = 20
  const [editing, setEditing] = useState<AdminGroup | null>(null)
  const [creating, setCreating] = useState(false)
  const [deleting, setDeleting] = useState<AdminGroup | null>(null)
  const [discounting, setDiscounting] = useState<AdminGroup | null>(null)

  const groupsQuery = useQuery({
    queryKey: [...GROUPS_KEY, page],
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/groups', {
          params: { query: { p: page, page_size: pageSize } },
        }),
      ),
  })

  const remove = useMutation({
    mutationFn: async (name: string) => {
      await apiClient.DELETE('/api/groups/{name}', { params: { path: { name } } })
    },
    onSuccess: async () => {
      push({ tone: 'success', message: t('admin.groups.deleted') })
      setDeleting(null)
      await queryClient.invalidateQueries({ queryKey: GROUPS_KEY })
    },
    onError: (error) => {
      push({
        tone: 'error',
        message:
          error instanceof ApiError && error.status === 409
            ? t('admin.groups.delete_error')
            : t('admin.groups.error_save'),
      })
    },
  })

  const items = groupsQuery.data?.items ?? []
  const total = groupsQuery.data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / pageSize))

  // The server refuses to delete any referenced group with 409 (R-38 keeps
  // the backend semantics). Mirror the three exposed reference counters in the
  // UI so the operator gets the reason before confirming; token references
  // are not exposed, which is why the 409 copy still exists as a fallback.
  function isReferenced(group: AdminGroup): boolean {
    return (
      group.users_count > 0 ||
      group.pricing_overrides_count > 0 ||
      group.channels_count > 0
    )
  }

  return (
    <div className="mx-auto flex max-w-[1520px] flex-col gap-3">
      <section className="rounded-xl border border-border-hairline bg-surface-card">
        <div className="flex items-center justify-between border-b border-border-hairline px-5 py-3">
          <h1 className="text-page-title font-semibold text-ink-1">{t('admin.groups.title')}</h1>
          <Button onClick={() => setCreating(true)}>{t('admin.groups.add')}</Button>
        </div>

        {groupsQuery.isPending ? (
          <div className="space-y-2 p-5" aria-busy="true">
            {[0, 1, 2, 3, 4].map((index) => (
              <div key={index} className="h-10 animate-pulse rounded bg-surface-subtle" />
            ))}
          </div>
        ) : groupsQuery.isError ? (
          <div className="flex items-center gap-3 p-5 text-body text-ink-2">
            <span>{t('admin.groups.error')}</span>
            <Button variant="secondary" onClick={() => void groupsQuery.refetch()}>
              {t('common.action.retry')}
            </Button>
          </div>
        ) : items.length === 0 ? (
          <p className="p-5 text-body text-ink-2">{t('admin.groups.empty')}</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[720px] text-body">
              <thead>
                <tr className="border-b border-border-hairline text-left text-meta text-ink-2">
                  <th scope="col" className="px-4 py-2 font-medium">{t('admin.groups.name')}</th>
                  <th scope="col" className="px-4 py-2 font-medium">{t('admin.groups.description')}</th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.groups.users')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.groups.pricing_overrides')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.groups.actions')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {items.map((group) => (
                  <tr
                    key={group.name}
                    className="border-b border-border-hairline last:border-0 hover:bg-surface-hover"
                  >
                    <td className="px-4 py-3">
                      <span className="font-mono text-ink-1">{group.name}</span>{' '}
                      {group.is_system ? (
                        <Tag variant="neutral">{t('admin.groups.system')}</Tag>
                      ) : null}
                    </td>
                    <td className="px-4 py-3 text-ink-2">{group.description ?? '—'}</td>
                    <td className="px-4 py-3 text-right font-mono tabular-nums">
                      {formatCredits(group.users_count)}
                    </td>
                    <td className="px-4 py-3 text-right font-mono tabular-nums">
                      {formatCredits(group.pricing_overrides_count)}
                    </td>
                    <td className="px-4 py-3">
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          className="h-8 px-2 text-meta"
                          onClick={() => setEditing(group)}
                        >
                          {t('admin.groups.edit')}
                        </Button>
                        <Button
                          variant="ghost"
                          className="h-8 px-2 text-meta"
                          onClick={() => setDiscounting(group)}
                        >
                          {t('admin.groups.discount')}
                        </Button>
                        <Button
                          variant="ghost"
                          className="h-8 px-2 text-meta text-danger hover:bg-danger-bg"
                          disabled={group.is_system || isReferenced(group)}
                          title={
                            group.is_system
                              ? t('admin.groups.system_guard')
                              : isReferenced(group)
                                ? t('admin.groups.referenced_guard')
                                : undefined
                          }
                          onClick={() => setDeleting(group)}
                        >
                          {t('admin.groups.delete')}
                        </Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        <div className="flex items-center justify-end gap-3 border-t border-border-hairline px-5 py-3 text-meta text-ink-2">
          <Button
            variant="secondary"
            className="h-8 px-2 text-meta"
            disabled={page <= 1}
            onClick={() => setPage((current) => Math.max(1, current - 1))}
          >
            {t('admin.users.pagination_previous')}
          </Button>
          <span className="font-mono tabular-nums">
            {t('admin.users.pagination_page', { page, pages })}
          </span>
          <Button
            variant="secondary"
            className="h-8 px-2 text-meta"
            disabled={page >= pages}
            onClick={() => setPage((current) => current + 1)}
          >
            {t('admin.users.pagination_next')}
          </Button>
        </div>
      </section>

      <GroupMutateDialog
        open={creating || editing !== null}
        group={editing}
        onOpenChange={(open) => {
          if (!open) {
            setCreating(false)
            setEditing(null)
          }
        }}
      />

      <GroupDiscountSheet
        group={discounting}
        onOpenChange={(open) => {
          if (!open) setDiscounting(null)
        }}
      />

      <Dialog
        open={deleting !== null}
        onOpenChange={(open) => {
          if (!open) setDeleting(null)
        }}
        title={t('admin.groups.delete_title')}
        description={
          deleting === null
            ? undefined
            : t('admin.groups.delete_body', { name: deleting.name })
        }
        footer={
          <>
            <Button variant="secondary" onClick={() => setDeleting(null)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              variant="danger"
              disabled={remove.isPending}
              onClick={() => {
                if (deleting !== null) remove.mutate(deleting.name)
              }}
            >
              {t('admin.groups.delete')}
            </Button>
          </>
        }
      />
    </div>
  )
}

interface GroupDiscountSheetProps {
  group: AdminGroup | null
  onOpenChange: (open: boolean) => void
}

/**
 * Per-model discounts for a group. Discount rules are billing records, not
 * fields of the group record, so `is_system` groups get this editor too (the
 * server remains the authority on who may write them). The table owns its own
 * queries, so it is mounted only while the sheet is open — a closed sheet
 * issues no requests.
 */
function GroupDiscountSheet({ group, onOpenChange }: GroupDiscountSheetProps) {
  const { t } = useI18n()
  return (
    <Sheet
      open={group !== null}
      onOpenChange={onOpenChange}
      title={
        group === null
          ? ''
          : t('admin.groups.discount_title', { name: group.name })
      }
      description={t('admin.groups.discount_description')}
      widthClassName="max-w-[900px]"
    >
      {group === null ? null : (
        <DiscountRuleTable targetType="group" targetID={group.name} />
      )}
    </Sheet>
  )
}

interface GroupMutateDialogProps {
  open: boolean
  group: AdminGroup | null
  onOpenChange: (open: boolean) => void
}

function GroupMutateDialog({ open, group, onOpenChange }: GroupMutateDialogProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [nameError, setNameError] = useState<string | undefined>(undefined)
  const [error, setError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    if (!open) return
    setName(group?.name ?? '')
    setDescription(group?.description ?? '')
    setNameError(undefined)
    setError(undefined)
  }, [open, group])

  async function save(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    // Client-side mirror of the contract so the field error appears instantly;
    // the server remains authoritative.
    if (!GROUP_NAME_PATTERN.test(name.trim())) {
      setNameError(t('admin.groups.name_invalid'))
      return
    }
    setNameError(undefined)
    setError(undefined)
    setSubmitting(true)
    const renamed = group !== null && name.trim() !== group.name
    try {
      if (group === null) {
        await apiClient.POST('/api/groups', {
          body: {
            name: name.trim(),
            description: description.trim() === '' ? null : description.trim(),
          },
        })
        push({ tone: 'success', message: t('admin.groups.created_toast') })
      } else {
        await apiClient.PATCH('/api/groups/{name}', {
          params: { path: { name: group.name } },
          body: {
            ...(name.trim() !== group.name ? { name: name.trim() } : {}),
            description: description.trim() === '' ? null : description.trim(),
          },
        })
        push({ tone: 'success', message: t('admin.groups.saved_toast') })
      }
      await queryClient.invalidateQueries({ queryKey: GROUPS_KEY })
      if (renamed) {
        // A rename is reflected on the affected users' rows (the server writes
        // the new group name through), so those list caches are stale too.
        await queryClient.invalidateQueries({ queryKey: ['admin-users'] })
      }
      onOpenChange(false)
    } catch {
      setError(t('admin.groups.error_save'))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={
        group === null ? t('admin.groups.create_title') : t('admin.groups.edit_title')
      }
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            {t('common.action.cancel')}
          </Button>
          <Button type="submit" form="group-mutate-form" disabled={submitting}>
            {t('admin.groups.save')}
          </Button>
        </>
      }
    >
      <form
        id="group-mutate-form"
        className="flex flex-col gap-3"
        onSubmit={(event) => void save(event)}
      >
        {error !== undefined ? <FormAlert>{error}</FormAlert> : null}
        <TextField
          label={t('admin.groups.name_label')}
          value={name}
          disabled={group?.is_system ?? false}
          onChange={(event) => {
            setName(event.target.value)
            setNameError(undefined)
          }}
          error={nameError}
          required
        />
        <TextField
          label={t('admin.groups.description_label')}
          value={description}
          onChange={(event) => {
            setDescription(event.target.value)
          }}
        />
      </form>
    </Dialog>
  )
}
