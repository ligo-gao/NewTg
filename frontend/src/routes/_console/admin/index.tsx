import { createFileRoute, redirect } from '@tanstack/react-router'

/**
 * `/admin` is the admin layout's mount point, not a page. Without this index
 * the exact path renders an empty console outlet; send it to the user list.
 * The parent `_console/admin` guard still runs first, so non-admins land on
 * `/403` before this redirect is considered.
 */
export const Route = createFileRoute('/_console/admin/')({
  beforeLoad: () => {
    throw redirect({ to: '/admin/users' })
  },
})
