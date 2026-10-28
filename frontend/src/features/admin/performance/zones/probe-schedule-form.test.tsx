import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { useState } from 'react'
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
const POLLED_UPDATED_AT = '2026-10-28T03:00:00Z'

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

/**
 * 带「父层轮询」的表单：页面每 60s 重拉 settings section，这里用一个按钮模拟
 * 那次重渲染，把新的 `section`（通常是新的 `updated_at`）传下去。
 */
async function renderPollingForm(
  options: {
    section?: SettingsSectionResponse
    polledSection?: SettingsSectionResponse
    config?: PerfConfig
    canManage?: boolean
  } = {},
) {
  const onSaved = vi.fn()
  const initial = options.section ?? section()
  const polled = options.polledSection ?? initial

  function Harness() {
    const [current, setCurrent] = useState(initial)
    return (
      <>
        <button type="button" onClick={() => setCurrent(polled)}>
          Simulate a poll
        </button>
        <ProbeScheduleForm
          section={current}
          config={options.config ?? config()}
          canManage={options.canManage ?? true}
          onSaved={onSaved}
        />
      </>
    )
  }

  const view = await renderWithProviders(<Harness />, ['admin'])
  return { ...view, onSaved }
}

function polledSection(): SettingsSectionResponse {
  return { section: 'perf', updated_at: POLLED_UPDATED_AT, items: savedItems() }
}

function conflictResponse(): Response {
  return HttpResponse.json(
    {
      type: 'https://newtg.dev/problems/conflict',
      title: 'Conflict',
      detail: 'settings version mismatch',
    },
    { status: 409 },
  )
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

  it('keeps the user input, the old token and offers a discard action on 409', async () => {
    const saves = installSave(conflictResponse)
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
    // 409 旁边必须有显式出口，不能只让用户自己猜该怎么刷新。
    expect(
      screen.getByRole('button', { name: 'Discard my edits and reload' }),
    ).toBeTruthy()

    // 旧令牌保留：再次保存仍带着首次变脏时的 updated_at，因此仍会 409。
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(saves).toHaveLength(2))
    expect(saves[1]?.body.expect_updated_at).toBe(UPDATED_AT)
  })

  it('snapshots expect_updated_at at the first edit, not at save time', async () => {
    const saves = installSave(successResponse)
    await renderPollingForm({ polledSection: polledSection() })

    fireEvent.change(numberInput('Probe interval'), {
      target: { value: '6' },
    })
    // 页面轮询拿到新的 updated_at —— 未保存的草稿必须继续用旧令牌。
    fireEvent.click(screen.getByRole('button', { name: 'Simulate a poll' }))
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(saves).toHaveLength(1))
    expect(saves[0]?.body.expect_updated_at).toBe(UPDATED_AT)
  })

  it('discards the draft on request and then saves against the refreshed version', async () => {
    let calls = 0
    const saves = installSave(() => {
      calls += 1
      return calls === 1 ? conflictResponse() : successResponse()
    })
    const { onSaved } = await renderPollingForm({
      polledSection: polledSection(),
    })

    fireEvent.change(numberInput('Runs per round'), { target: { value: '5' } })
    fireEvent.click(screen.getByRole('button', { name: 'Simulate a poll' }))
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    await screen.findByText(
      'Settings were changed elsewhere. Refresh and try again.',
    )

    fireEvent.click(
      screen.getByRole('button', { name: 'Discard my edits and reload' }),
    )

    // 草稿清空（输入框回到基线）并让父层重拉；令牌一起清空。
    expect(numberInput('Runs per round').value).toBe('3')
    expect(
      screen.queryByText(
        'Settings were changed elsewhere. Refresh and try again.',
      ),
    ).toBeNull()
    expect(onSaved).toHaveBeenCalledTimes(1)

    // 令牌清空后再次变脏，快照的是刷新后的 updated_at。
    fireEvent.change(numberInput('Runs per round'), { target: { value: '6' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(saves).toHaveLength(2))
    expect(saves[1]?.body.expect_updated_at).toBe(POLLED_UPDATED_AT)
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
    // TextField 自带 error 段落关联：错误文案要能被读屏读到。
    const describedBy = numberInput('Runs per round').getAttribute(
      'aria-describedby',
    )
    expect(describedBy).not.toBeNull()
    expect(document.getElementById(describedBy as string)?.textContent).toBe(
      'Must be at most 5.',
    )
  })

  it('links the enabled switch to its error text and drops the link once cleared', async () => {
    installSave(() =>
      HttpResponse.json(
        {
          type: 'https://newtg.dev/problems/validation_failed',
          title: 'Validation failed',
          errors: [
            {
              field: 'perf.schedule.enabled',
              message: 'The schedule cannot be turned off.',
            },
          ],
        },
        { status: 422 },
      ),
    )
    await renderForm()

    const toggle = screen.getByRole('switch')
    expect(toggle.getAttribute('aria-describedby')).toBeNull()

    fireEvent.click(toggle)
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    const message = await screen.findByText(
      'The schedule cannot be turned off.',
    )
    expect(message.id).toBe('probe-schedule-enabled-error')
    expect(toggle.getAttribute('aria-describedby')).toBe(
      'probe-schedule-enabled-error',
    )

    // 再次改动该字段会清掉错误，关联也要随之撤销。
    fireEvent.click(toggle)
    expect(toggle.getAttribute('aria-describedby')).toBeNull()
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

  it('reports an unknown next run when the schedule is enabled without a timestamp', async () => {
    await renderForm({
      config: config({
        schedule: schedule({ enabled: true, next_run_at: null }),
      }),
    })

    // 启用但没有时间戳 ≠ 已停用：调度器算不出下一次运行。
    const footer = screen.getByTestId('probe-next-run')
    expect(footer.textContent).toBe(
      'Next run: unknown — check the probe interval.',
    )
    expect(footer.textContent).not.toBe('Disabled')
  })

  it.each([
    [
      'settings',
      'This page',
      'Running on values saved on this page; PERF_SCHEDULE_* no longer applies.',
      ['Environment variables', 'Mixed sources'],
    ],
    [
      'env',
      'Environment variables',
      'Not saved on this page yet; currently running from PERF_SCHEDULE_*: every 12 h (—) · 3 runs · 2 concurrent. Saving once makes this page authoritative.',
      ['This page', 'Mixed sources'],
    ],
    [
      'mixed',
      'Mixed sources',
      'Some values come from this page, others from environment variables; saving once makes this page authoritative.',
      ['This page', 'Environment variables'],
    ],
  ] as const)(
    'maps source %s to its badge and note',
    async (source, badge, note, absentBadges) => {
      await renderForm({
        config: config({ schedule: schedule({ source }) }),
      })

      const footer = screen.getByTestId('probe-source')
      expect(footer.textContent).toContain(badge)
      expect(footer.textContent).toContain(note)
      // 三态徽标互斥：另两句徽标文案不能同时出现。
      for (const absent of absentBadges) {
        expect(footer.textContent).not.toContain(absent)
      }
    },
  )

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

    expect(screen.getByTestId('probe-schedule-readonly').textContent).toBe(
      'Rows marked "Restart required" come from environment variables: change them in the deployment config, then restart the service. Rows marked "Code change required" are built into the code and only change with a new build.',
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

  async function renderZone(
    overrides: Partial<PerfConfig> = {},
    zoneSection: SettingsSectionResponse = section(),
  ) {
    const onSaved = vi.fn()
    const view = await renderWithProviders(
      <SettingsZone
        section={zoneSection}
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

  it('renders the zone rail as the visible heading with the editable key count', async () => {
    await renderZone()

    const heading = screen.getByRole('heading', { name: 'Runtime settings' })
    expect(heading.tagName).toBe('H2')
    // 区块的无障碍名来自这个可见标题，而不是另一份重复文案。
    expect(
      screen.getByTestId('settings-zone').getAttribute('aria-labelledby'),
    ).toBe(heading.id)
    expect(screen.getByText('4 editable online')).toBeTruthy()
    expect(
      screen.getByText(
        'Saved values are written to the config center; every replica applies them within 5 minutes, no restart required.',
      ),
    ).toBeTruthy()
  })

  it('counts only the schedule keys that exist in the section', async () => {
    await renderZone(
      {},
      section(
        savedItems().filter(
          (entry) => entry.key !== 'perf.schedule.concurrency',
        ),
      ),
    )

    expect(screen.getByText('3 editable online')).toBeTruthy()
  })

  it('localises the cache-ttl and log-pattern rows in server order', async () => {
    await renderZone({
      env: [
        {
          key: 'PROMETHEUS_TIMEOUT',
          value: '5s',
          configured: true,
          set: true,
          effect: 'restart',
        },
        {
          key: 'PROMETHEUS_CACHE_TTL',
          value: '15s',
          configured: true,
          set: true,
          effect: 'restart',
        },
        {
          key: 'LOG_DIR',
          value: '/var/log/newtg',
          configured: true,
          set: true,
          effect: 'restart',
        },
        {
          key: 'LOG_FILE_PATTERN',
          value: 'newtg-*.log',
          configured: true,
          set: true,
          effect: 'restart',
        },
      ],
    })

    // 只读区有专用标题，不再复用表单里的「只读」徽标文案。
    expect(
      screen.getByRole('heading', {
        name: 'Deployment parameters (read-only)',
      }),
    ).toBeTruthy()

    const env = screen.getByTestId('settings-env-table')
    const labels = within(env)
      .getAllByRole('rowheader')
      .map((cell) => cell.textContent)
    // 顺序即服务端顺序：新增两行紧跟各自的邻居，不靠前端特判。
    expect(labels).toEqual([
      'Monitoring query timeout',
      'Monitoring query cache',
      'Log directory',
      'Log file pattern',
    ])
    // 原始键名出现即说明语言键没命中。
    expect(within(env).queryByText('PROMETHEUS_CACHE_TTL')).toBeNull()
    expect(within(env).queryByText('LOG_FILE_PATTERN')).toBeNull()
  })
})
