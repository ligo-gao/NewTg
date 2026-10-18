import { createFileRoute } from '@tanstack/react-router'

import { NotFoundPage } from '@/app/not-found-page'

export const Route = createFileRoute('/_errors/404')({
  component: NotFoundPage,
})
