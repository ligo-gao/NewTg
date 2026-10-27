import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

import type {
  SettingsItem,
  SettingsSectionResponse,
} from '@/features/admin/settings/api'
import { renderWithProviders } from '@/test-support/providers'
import { useToastStore } from '@/shared/ui/toast'

import type { PerfConfig } from '../api'
import { formatDateTime } from '../utils'
import { ProbeScheduleForm } from './probe-schedule-form'
import { SettingsZone } from './settings-zone'

const ORIGIN = 'http://localhost:3000'
const UPDATED_AT = '2026-10-28T01:00:00Z'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  vi.restoreAllMocks()
})
afterAll(() => server.close())

beforeEach(() => {
  useToastStore.setState({ toasts: [] })
})

type Schedule = NonNullable<PerfConfig['schedule']>

// 本地时间构造：断言不依赖跑测试的时区，也不让组件做任何日期运算。
const NEXT_RUN = new Date(2026, 9, 28, 12, 0, 0)

function schedule(overrides: Partial<Schedule> = {}): Schedule {
  return {
    enabled: true,
    interval_hours: 12,
    runs: 3,
    concurrency: 2,
    source: 'settings',
    timezone: 'Asia/Shanghai',
    next_run_at: NEXT_RUN.toISOString(),
    legacy_times: '',
    ...overrides,
  }
}

function config(overrides: Partial<PerfConfig> = {}): PerfConfig {
  return {
    generated_at: UPDATED_AT,
    schedule: schedule(),
    env: [],
    fixed: [],
    settings_groups: [],
    ...overrides,
  }
}

function item(
  key: string,
  value: unknown,
  defaultValue: unknown,
  overrides: Partial<SettingsItem> = {},
): SettingsItem {
  return {
    key,
    value,
    type: typeof value === 'boolean' ? 'bool' : 'int',
    default: defaultValue,
    public: false,
    secret: false,
    dangerous: false,
    ...overrides,
  }
}

function savedItems(): SettingsItem[] {
  return [
    item('perf.schedule.enabled', true, true),
    item('perf.schedule.interval_hours', 12, 12),
    item('perf.schedule.runs', 3, 3),
    item('perf.schedule.concurrency', 2, 2),
  ]
}

function section(items: SettingsItem[] = savedItems()): SettingsSectionResponse {
  return { section: 'perf', updated_at: UPDATED_AT, items }
}

interface SaveLog {
  body: {
    values: Record<string, unknown>
    expect_updated_at?: string | null
  }
}

function installSave(
  handler: (log: SaveLog) => Response | Promise<Response>,
): SaveLog[] {
  const saves: SaveLog[] = []
  server.use(
    http.put(`${ORIGIN}/api/settings`, async ({ request }) => {
      const body = (await request.json()) as SaveLog['body']
      const log: SaveLog = { body }
      saves.push(log)
      return handler(log)
    }),
  )
  return saves
}

function successResponse(): Response {
  return HttpResponse.json({
    section: 'perf',
    updated_at: '2026-10-28T02:00:00Z',
    values: {},
  })
}

async function renderForm(options: {
  section?: SettingsSectionResponse
  config?: PerfConfig
  canManage?: boolean
  onRunNow?: () => void
} = {}) {
  const onSaved = vi.fn()
  const view = await renderWithProviders(
    <ProbeScheduleForm
      section={options.section ?? section()}
      config={options.config ?? config()}
      canManage={options.canManage ?? true}
      onSaved={onSaved}
      onRunNow={options.onRunNow}
    />,
    ['admin'],
  )
  return { ...view, onSaved }
}

function numberInput(label: string): HTMLInputElement {
  return screen.getByLabelText(label) as HTMLInputElement
}

describe('ProbeScheduleForm', () => {
  it('shows the registry default and the environment source when a key is unsaved', async () => {
    await renderForm({
      section: section([
        item('perf.schedule.enabled', null, true),
        item('perf.schedule.interval_hours', null, 12),
        item('perf.schedule.runs', null, 3),
        item('perf.schedule.concurrency', null, 2),
      ]),
      config: config({ schedule: schedule({ source: 'env' }) }),
    })

    expect(numberInput('Probe interval').value).toBe('12')
    expect(numberInput('Runs per round').value).toBe('3')

    const source = screen.getByTestId('probe-source')
    expect(source.textContent).toContain('Environment variables')
    expect(source.textContent).toContain('PERF_SCHEDULE_*')
    expect(source.textContent).toContain(
      'every 12 h (—) · 3 runs · 2 concurrent',
    )
  })

  it('submits only the changed keys plus expect_updated_at, then toasts and notifies the page', async () => {
    const saves = installSave(successResponse)
    const { onSaved } = await renderForm()

    fireEvent.change(numberInput('Probe interval'), {
      target: { value: '6' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(saves).toHaveLength(1))
    expect(saves[0]?.body.values).toEqual({
      'perf.schedule.interval_hours': 6,
    })
    expect(saves[0]?.body.expect_updated_at).toBe(UPDATED_AT)

    expect(onSaved).toHaveBeenCalledTimes(1)
    expect(useToastStore.getState().toasts.at(-1)?.message).toBe('Saved')
  })

  it('keeps the user input and shows conflict copy on 409', async () => {
    installSave(() =>
      HttpResponse.json(
        {
          type: 'https://newtg.dev/problems/conflict',
          title: 'Conflict',
          detail: 'settings version mismatch',
        },
        { status: 409 },
      ),
    )
    const { onSaved } = await renderForm()

    fireEvent.change(numberInput('Runs per round'), { target: { value: '5' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(
      await screen.findByText(
        'Settings were changed elsewhere. Refresh and try again.',
      ),
    ).toBeTruthy()
    // 不静默覆盖、也不自动重拉替换用户输入。
    expect(numberInput('Runs per round').value).toBe('5')
    expect(onSaved).not.toHaveBeenCalled()
  })

  it('attaches a server 422 field error to the matching input', async () => {
    installSave(() =>
      HttpResponse.json(
        {
          type: 'https://newtg.dev/problems/validation_failed',
          title: 'Validation failed',
          errors: [
            { field: 'perf.schedule.runs', message: 'Must be at most 5.' },
          ],
        },
        { status: 422 },
      ),
    )
    await renderForm()

    fireEvent.change(numberInput('Runs per round'), { target: { value: '9' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(await screen.findByText('Must be at most 5.')).toBeTruthy()
    expect(numberInput('Runs per round').value).toBe('9')
  })

  it('renders the server-provided next run time without client arithmetic', async () => {
    await renderForm()

    const expected = formatDateTime(NEXT_RUN.toISOString(), 'en')
    const footer = screen.getByTestId('probe-next-run')
    expect(footer.textContent).toBe(`Next run ${expected}`)
    // 服务端给的 12:00 原样出现；前端不加减、不推断。
    expect(footer.textContent).toContain('12:00')
  })

  it('shows the disabled footer when the schedule is off', async () => {
    await renderForm({
      config: config({
        schedule: schedule({ enabled: false, next_run_at: null }),
      }),
    })

    expect(screen.getByTestId('probe-next-run').textContent).toBe('Disabled')
  })

  it.each([
    [
      'settings',
      'This page',
      'Running on values saved on this page; PERF_SCHEDULE_* no longer applies.',
    ],
    [
      'mixed',
      'Mixed sources',
      'Some values come from this page, others from environment variables; saving once makes this page authoritative.',
    ],
  ] as const)('maps source %s to its badge and note', async (source, badge, note) => {
    await renderForm({
      config: config({ schedule: schedule({ source }) }),
    })

    const footer = screen.getByTestId('probe-source')
    expect(footer.textContent).toContain(badge)
    expect(footer.textContent).toContain(note)
  })

  it('disables a field that is absent from the section and labels it read-only', async () => {
    await renderForm({
      section: section(
        savedItems().filter(
          (entry) => entry.key !== 'perf.schedule.concurrency',
        ),
      ),
    })

    expect(numberInput('Concurrent lanes').disabled).toBe(true)
    expect(screen.getAllByText('Read-only').length).toBeGreaterThan(0)
  })

  it('renders a read-only notice instead of the form when no scheduler is wired', async () => {
    await renderForm({ config: config({ schedule: null }) })

    expect(screen.getByTestId('probe-schedule-readonly').textContent).toContain(
      'Comes from environment variables or built into the code; takes effect only by changing the deployment config and rebuilding.',
    )
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull()
    expect(screen.queryByRole('switch')).toBeNull()
  })

  it('is entirely read-only without settings:manage', async () => {
    await renderForm({ canManage: false })

    expect(numberInput('Probe interval').disabled).toBe(true)
    expect(numberInput('Runs per round').disabled).toBe(true)
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull()
    expect(screen.getAllByText('Read-only').length).toBeGreaterThan(0)
  })

  it('renders the optional run-now action only when the page supplies it', async () => {
    const onRunNow = vi.fn()
    await renderForm({ onRunNow })

    fireEvent.click(screen.getByRole('button', { name: 'Run a round now' }))
    expect(onRunNow).toHaveBeenCalledTimes(1)
  })
})

describe('SettingsZone', () => {
  const GROUPS: PerfConfig['settings_groups'] = [
    {
      id: 'channel_health',
      tab: 'policy',
      link: '/admin/settings?tab=policy',
      items: [
        {
          key: 'rl.health.test_interval_minutes',
          value: 10,
          type: 'int',
          unit: 'minutes',
          source: 'settings',
        },
        {
          key: 'rl.health.test_concurrency',
          value: 5,
          type: 'int',
          unit: 'count',
          source: 'settings',
        },
      ],
    },
    {
      id: 'rate_limit',
      tab: 'security',
      link: '/admin/settings?tab=security',
      items: [
        {
          key: 'rl.user_model_rpm',
          value: 60,
          type: 'int',
          unit: 'rpm',
          source: 'default',
        },
      ],
    },
    {
      id: 'retention',
      tab: '',
      link: '',
      items: [
        {
          key: 'log.partition_drop_enabled',
          value: false,
          type: 'bool',
          unit: 'onoff',
          source: 'default',
        },
      ],
    },
  ]

  async function renderZone(overrides: Partial<PerfConfig> = {}) {
    const onSaved = vi.fn()
    const view = await renderWithProviders(
      <SettingsZone
        section={section()}
        config={config({ settings_groups: GROUPS, ...overrides })}
        canManage
        onSaved={onSaved}
      />,
      ['admin'],
    )
    return { ...view, onSaved }
  }

  it('renders each business group with translated titles, values, units and links', async () => {
    await renderZone()

    expect(screen.getByText('Channel health probing')).toBeTruthy()
    expect(screen.getByText('Rate limiting & timeouts')).toBeTruthy()
    expect(screen.getByText('Retention & cleanup')).toBeTruthy()

    const health = document.querySelector<HTMLElement>(
      '[data-group="channel_health"]',
    ) as HTMLElement
    expect(within(health).getByText('Probe interval')).toBeTruthy()
    expect(within(health).getByText('10 min')).toBeTruthy()
    // unit === count 不拼单位（demo 是裸数字）。
    expect(within(health).getByText('5')).toBeTruthy()
    expect(screen.queryByText('5 items')).toBeNull()
    expect(screen.getByText('60 RPM')).toBeTruthy()

    const links = screen.getAllByRole('link', { name: 'Change in settings' })
    expect(links.map((link) => link.getAttribute('href'))).toEqual([
      '/admin/settings?tab=policy',
      '/admin/settings?tab=security',
    ])
    // 无 tab 的组只给值，不给死链。
    const retention = document.querySelector<HTMLElement>(
      '[data-group="retention"]',
    )
    expect(retention).not.toBeNull()
    expect(within(retention as HTMLElement).queryByRole('link')).toBeNull()
  })

  it('renders the dangerous partition switch as a badge and never as a control', async () => {
    await renderZone()

    const retention = document.querySelector<HTMLElement>(
      '[data-group="retention"]',
    ) as HTMLElement
    expect(within(retention).getByText('Off')).toBeTruthy()
    expect(within(retention).queryByRole('checkbox')).toBeNull()
    expect(within(retention).queryByRole('switch')).toBeNull()
    expect(within(retention).queryByRole('button')).toBeNull()
  })

  it('renders the read-only env and fixed tables with the frozen columns', async () => {
    await renderZone({
      env: [
        {
          key: 'PROMETHEUS_URL',
          value: 'http://prometheus:9090',
          configured: true,
          set: true,
          effect: 'restart',
        },
        {
          key: 'METRICS_TOKEN',
          configured: true,
          set: true,
          effect: 'restart',
        },
        {
          key: 'LOG_DIR',
          value: '',
          configured: false,
          set: false,
          effect: 'restart',
        },
        {
          key: 'NEW_KNOB',
          value: 'on',
          configured: true,
          set: true,
          effect: 'restart',
        },
      ],
      fixed: [
        {
          key: 'probe_timeout',
          value: '30s',
          unit: 'duration',
          effect: 'code',
        },
      ],
    })

    const env = screen.getByTestId('settings-env-table')
    const headers = within(env)
      .getAllByRole('columnheader')
      .map((header) => header.textContent)
    expect(headers).toEqual(['Parameter', 'Current value', 'Source', 'Effect'])

    expect(within(env).getByText('Monitoring query address')).toBeTruthy()
    expect(within(env).getByText('http://prometheus:9090')).toBeTruthy()
    expect(within(env).getByText('Set')).toBeTruthy()
    expect(within(env).getAllByText('Not configured').length).toBeGreaterThan(0)
    expect(within(env).getAllByText('Environment variable')).toHaveLength(4)
    expect(within(env).getAllByText('Restart required')).toHaveLength(4)
    // 未登记的语言键回退原始键名，而不是硬编码英文标签。
    expect(within(env).getByText('NEW_KNOB')).toBeTruthy()

    const fixed = screen.getByTestId('settings-fixed-table')
    expect(within(fixed).getByText('Probe timeout')).toBeTruthy()
    expect(within(fixed).getByText('30s')).toBeTruthy()
    expect(within(fixed).getByText('Default')).toBeTruthy()
    expect(within(fixed).getByText('Code change required')).toBeTruthy()
  })
})
