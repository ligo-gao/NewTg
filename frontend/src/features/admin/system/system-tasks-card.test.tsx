import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'
import { ToastViewport } from '@/shared/ui/toast'

import { SystemTasksCard } from './system-tasks-card'
import type { SystemTaskPage } from './system-tasks-api'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const TASK_PAGE: SystemTaskPage = {
  items: [
    {
      task_type: 'worker_history_cleanup',
      description: 'Delete worker task run history beyond the retention window',
      enabled: true,
      interval_seconds: 86_400,
      last_run_at: '2026-10-22T09:00:00Z',
      last_status: 'succeeded',
      next_due_at: '2026-10-23T09:00:00Z',
      running: false,
      last_duration_ms: 1200,
      manual_trigger_allowed: true,
      concurrency: 'single',
    },
    {
      task_type: 'async_task_poll',
      description: 'Poll unfinished plugin-family tasks upstream',
      enabled: true,
      interval_seconds: 15,
      last_run_at: null,
      last_status: null,
      next_due_at: null,
      running: true,
      holder_id: 'worker-1',
      last_duration_ms: null,
      manual_trigger_allowed: false,
      concurrency: 'single',
    },
  ],
  total: 2,
  page: 1,
  page_size: 20,
}

async function renderCard() {
  return renderWithProviders(
    <>
      <SystemTasksCard />
      <ToastViewport />
    </>,
    ['common', 'settings'],
  )
}

describe('SystemTasksCard', () => {
  it('renders the registry and only exposes trigger for allowlisted tasks', async () => {
    server.use(
      http.get(`${ORIGIN}/api/system/tasks`, () => HttpResponse.json(TASK_PAGE)),
    )
    await renderCard()

    expect(await screen.findByText('worker_history_cleanup')).toBeTruthy()
    expect(screen.getByText('async_task_poll')).toBeTruthy()
    // 非白名单任务没有触发入口（TC-F-08-21-06 的 UI 半边）。
    expect(screen.getAllByRole('button', { name: 'Trigger now' })).toHaveLength(1)
    // 运行中的任务显示持有者与运行状态。
    expect(screen.getByText('worker-1')).toBeTruthy()
    expect(screen.getAllByText('Running').length).toBeGreaterThan(0)
    expect(screen.getByText('1.2s')).toBeTruthy()
  })

  it('expands a task and shows recent runs', async () => {
    const user = userEvent.setup()
    server.use(
      http.get(`${ORIGIN}/api/system/tasks`, () => HttpResponse.json(TASK_PAGE)),
      http.get(`${ORIGIN}/api/system/tasks/worker_history_cleanup`, () =>
        HttpResponse.json({
          summary: TASK_PAGE.items[0],
          runs: {
            items: [
              {
                id: 9,
                task_type: 'worker_history_cleanup',
                status: 'failed',
                node_id: 'worker-1',
                started_at: '2026-10-22T09:00:00Z',
                finished_at: '2026-10-22T09:00:01Z',
                error: 'timeout',
                result: null,
                trigger: 'manual',
              },
            ],
            total: 1,
            page: 1,
            page_size: 20,
          },
        }),
      ),
    )
    await renderCard()
    await screen.findByText('worker_history_cleanup')

    const detailsButtons = screen.getAllByRole('button', { name: 'Details' })
    const firstDetails = detailsButtons[0]
    if (firstDetails === undefined) throw new Error('missing Details button')
    await user.click(firstDetails)
    expect(await screen.findByText('Recent runs')).toBeTruthy()
    expect(screen.getByText('timeout')).toBeTruthy()
    expect(screen.getByText('Manual')).toBeTruthy()
  })

  it('requires confirmation before triggering and enqueues with an idempotency key', async () => {
    const user = userEvent.setup()
    let postedKey = ''
    server.use(
      http.get(`${ORIGIN}/api/system/tasks`, () => HttpResponse.json(TASK_PAGE)),
      http.post(/\/api\/system\/tasks\/worker_history_cleanup:trigger$/, ({ request }) => {
        postedKey = request.headers.get('Idempotency-Key') ?? ''
        return HttpResponse.json(
          { run_id: 42, task_type: 'worker_history_cleanup', trigger: 'manual' },
          { status: 202 },
        )
      }),
    )
    await renderCard()
    await screen.findByText('worker_history_cleanup')

    await user.click(screen.getByRole('button', { name: 'Trigger now' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('Trigger this task?')).toBeTruthy()
    // 二次确认：未确认前不得发请求。
    expect(postedKey).toBe('')

    await user.click(within(dialog).getByRole('button', { name: 'Trigger' }))
    expect(await screen.findByText(/Task enqueued/)).toBeTruthy()
    expect(postedKey).not.toBe('')
  })

  it('shows the 409 conflict copy when the task is already running', async () => {
    const user = userEvent.setup()
    server.use(
      http.get(`${ORIGIN}/api/system/tasks`, () => HttpResponse.json(TASK_PAGE)),
      http.post(/\/api\/system\/tasks\/worker_history_cleanup:trigger$/, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/task_already_running',
            title: 'Task is already running',
            status: 409,
          },
          { status: 409 },
        ),
      ),
    )
    await renderCard()
    await screen.findByText('worker_history_cleanup')

    await user.click(screen.getByRole('button', { name: 'Trigger now' }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Trigger' }))
    expect(await screen.findByText('The task is already running or queued.')).toBeTruthy()
  })

  it('hides the whole card without settings:manage', async () => {
    server.use(
      http.get(`${ORIGIN}/api/system/tasks`, () =>
        HttpResponse.json(
          { type: 'https://newtg.dev/problems/forbidden', title: 'Forbidden', status: 403 },
          { status: 403 },
        ),
      ),
    )
    await renderCard()
    await waitFor(() => {
      expect(screen.queryByTestId('system-tasks-card')).toBeNull()
    })
  })

  it('renders the empty state', async () => {
    server.use(
      http.get(`${ORIGIN}/api/system/tasks`, () =>
        HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
      ),
    )
    await renderCard()
    expect(await screen.findByText('No worker tasks are registered.')).toBeTruthy()
  })
})
