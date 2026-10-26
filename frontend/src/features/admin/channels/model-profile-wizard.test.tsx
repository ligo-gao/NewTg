import { fireEvent, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import type { components } from '@/api/schema'

import { fetchChannelModels } from './api'
import { ModelProfileWizard } from './model-profile-wizard'
import { CREATE_VENDOR } from './model-profile-wizard-step-defaults'
import { renderWithProviders } from './test-utils'
import type { FetchChannelModelEntry } from './types'

type ModelMeta = components['schemas']['ModelMeta']
type User = ReturnType<typeof userEvent.setup>

vi.mock('./api', () => ({
  fetchChannelModels: vi.fn(),
}))
vi.mock('@/api/client', () => ({
  apiClient: { GET: vi.fn(), POST: vi.fn(), PUT: vi.fn() },
}))

const mockFetch = vi.mocked(fetchChannelModels)
const mockGet = vi.mocked(apiClient.GET)
const mockPost = vi.mocked(apiClient.POST)
const mockPut = vi.mocked(apiClient.PUT)

const REQUEST = {
  type: 'gemini',
  base_url: 'https://generativelanguage.googleapis.com',
  credentials: { api_key: 'AIza-test' },
}

function entry(
  overrides: Partial<FetchChannelModelEntry> = {},
): FetchChannelModelEntry {
  return {
    id: 'gpt-4o',
    display_name: '',
    description: '',
    context_length: null,
    max_output: null,
    released_at: '',
    endpoints: [],
    vendor_suggestion: '',
    facts_source: '',
    ...overrides,
  }
}

function renderWizard() {
  return renderWithProviders(
    <ModelProfileWizard
      request={REQUEST}
      channelModels={[]}
      onBack={vi.fn()}
      onApplyModels={vi.fn()}
    />,
  )
}

beforeEach(() => {
  mockFetch.mockReset()
  mockGet.mockReset()
  mockPost.mockReset()
  mockPut.mockReset()
  mockGet.mockResolvedValue({ data: { items: [], total: 0 } } as never)
  mockPost.mockResolvedValue({ data: {} } as never)
  mockPut.mockResolvedValue({ data: {} } as never)
})

function modelMeta(overrides: Partial<ModelMeta> = {}): ModelMeta {
  return {
    id: 1,
    model_name: 'gpt-4o',
    display_name: null,
    description: null,
    icon: null,
    tags: [],
    vendor_id: null,
    endpoints: {},
    capabilities: [],
    modalities: [],
    parameters: [],
    context_length: null,
    max_output: null,
    knowledge_cutoff: null,
    released_at: null,
    status: 1,
    sync_official: true,
    name_rule: 0,
    created_at: '2026-10-01T00:00:00Z',
    updated_at: '2026-10-20T08:00:00Z',
    vendor_name: null,
    square_state: 'hidden',
    configured_channel_count: 0,
    enable_groups: [],
    ...overrides,
  }
}

function listResponse(items: unknown[]) {
  return { data: { items, total: items.length } }
}

/** `/api/models` 与 `/api/vendors` 按路径返回固定条目，其它路径空列表。 */
function mockCatalog(
  models: ModelMeta[],
  vendors: Array<{ id: number; name: string }> = [],
): void {
  mockGet.mockImplementation(((path: string) => {
    if (path === '/api/models') return Promise.resolve(listResponse(models))
    if (path === '/api/vendors') return Promise.resolve(listResponse(vendors))
    return Promise.resolve(listResponse([]))
  }) as never)
}

async function goToStep(user: User, step: 2 | 3): Promise<void> {
  await user.click(await screen.findByRole('button', { name: 'Next' }))
  if (step === 3) {
    await user.click(screen.getByRole('button', { name: 'Next' }))
  }
}

async function submitWizard(user: User): Promise<void> {
  await user.click(screen.getByRole('button', { name: /^Write \d+ profiles$/ }))
}

async function goToConfirmAndSubmit(user: User): Promise<void> {
  await goToStep(user, 3)
  await submitWizard(user)
}

describe('ModelProfileWizard', () => {
  it('probes the upstream and shows one row per advertised model', async () => {
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro', 'text-embedding-004'],
      entries: [
        entry({
          id: 'gemini-2.5-pro',
          display_name: 'Gemini 2.5 Pro',
          context_length: 1048576,
        }),
        entry({ id: 'text-embedding-004', endpoints: [] }),
      ],
    })

    renderWizard()

    expect(await screen.findByText('gemini-2.5-pro')).toBeTruthy()
    expect(screen.getByText('Gemini 2.5 Pro')).toBeTruthy()
    // Values the upstream supplied are marked "Auto"; the row that only has a
    // model id is marked "Missing".
    expect(screen.getAllByText('Auto').length).toBeGreaterThan(0)
    expect(screen.getAllByText('Missing').length).toBeGreaterThan(0)
    expect(screen.getByText('text-embedding-004')).toBeTruthy()
  })

  it('shows an empty state when the upstream advertises nothing', async () => {
    mockFetch.mockResolvedValue({ models: [], entries: [] })

    renderWizard()

    expect(
      await screen.findByText('The upstream returned no models.'),
    ).toBeTruthy()
    expect(
      screen.getByRole('button', { name: 'Back to channel form' }),
    ).toBeTruthy()
  })

  it('surfaces a probe failure with a retry action', async () => {
    mockFetch.mockRejectedValue(
      new ApiError({
        status: 502,
        type: 'https://newtg.dev/problems/upstream_error',
        title: 'Upstream error',
        detail: 'The upstream returned 502',
        kind: 'server',
      }),
    )

    renderWizard()

    expect(
      await screen.findByText('The upstream returned 502'),
    ).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy()
    expect(
      screen.getByRole('button', { name: 'Back to channel form' }),
    ).toBeTruthy()
  })

  it('does not write models back when returning from step 1', async () => {
    const user = userEvent.setup()
    const onBack = vi.fn()
    const onApplyModels = vi.fn()
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro', 'text-embedding-004'],
      entries: [
        entry({ id: 'gemini-2.5-pro' }),
        entry({ id: 'text-embedding-004' }),
      ],
    })

    renderWithProviders(
      <ModelProfileWizard
        request={REQUEST}
        channelModels={[]}
        onBack={onBack}
        onApplyModels={onApplyModels}
      />,
    )

    await screen.findByText('gemini-2.5-pro')
    await user.click(screen.getByRole('button', { name: 'Back to channel form' }))

    // 还没看过确认页，默认勾选不应直接并集写回 supported_models。
    expect(onApplyModels).not.toHaveBeenCalled()
    expect(onBack).toHaveBeenCalledTimes(1)
  })

  it('locks the next step when the model catalog fails to load', async () => {
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro'],
      entries: [entry({ id: 'gemini-2.5-pro' })],
    })
    mockGet.mockImplementation(((path: string) =>
      path === '/api/models'
        ? Promise.reject(new Error('catalog down'))
        : Promise.resolve({ data: { items: [], total: 0 } })) as never)

    renderWizard()

    expect(
      await screen.findByText(
        'Could not load the model catalog. Reload it before continuing.',
      ),
    ).toBeTruthy()
    expect(
      screen.getByRole('button', { name: 'Reload catalog' }),
    ).toBeTruthy()
    expect(
      (screen.getByRole('button', { name: 'Next' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
  })

  it('unlocks the next step once a failed catalog retry succeeds', async () => {
    const user = userEvent.setup()
    let catalogCalls = 0
    mockGet.mockImplementation(((path: string) => {
      if (path === '/api/models') {
        catalogCalls += 1
        return catalogCalls === 1
          ? Promise.reject(new Error('catalog down'))
          : Promise.resolve({ data: { items: [], total: 0 } })
      }
      return Promise.resolve({ data: { items: [], total: 0 } })
    }) as never)
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro'],
      entries: [entry({ id: 'gemini-2.5-pro' })],
    })

    renderWizard()

    await user.click(
      await screen.findByRole('button', { name: 'Reload catalog' }),
    )

    await waitFor(() => {
      expect(
        (screen.getByRole('button', { name: 'Next' }) as HTMLButtonElement)
          .disabled,
      ).toBe(false)
    })
  })

  it('keeps models the upstream did not advertise when writing back', async () => {
    const user = userEvent.setup()
    const onApplyModels = vi.fn()
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro', 'text-embedding-004'],
      entries: [
        entry({ id: 'gemini-2.5-pro' }),
        entry({ id: 'text-embedding-004' }),
      ],
    })

    renderWithProviders(
      <ModelProfileWizard
        request={REQUEST}
        channelModels={['legacy-model']}
        onBack={vi.fn()}
        onApplyModels={onApplyModels}
      />,
    )

    await screen.findByText('gemini-2.5-pro')
    await user.click(
      screen.getByRole('checkbox', { name: 'text-embedding-004' }),
    )
    // 写回只发生在结果页：先提交，再点「返回渠道表单」。
    await goToConfirmAndSubmit(user)
    await user.click(
      await screen.findByRole('button', { name: 'Back to channel form' }),
    )

    expect(onApplyModels).toHaveBeenCalledWith([
      'legacy-model',
      'gemini-2.5-pro',
    ])
  })

  it('leaves the channel model list untouched when the fill toggle is off', async () => {
    const user = userEvent.setup()
    const onApplyModels = vi.fn()
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro'],
      entries: [entry({ id: 'gemini-2.5-pro' })],
    })

    renderWithProviders(
      <ModelProfileWizard
        request={REQUEST}
        channelModels={[]}
        onBack={vi.fn()}
        onApplyModels={onApplyModels}
      />,
    )

    await screen.findByText('gemini-2.5-pro')
    await user.click(
      screen.getByRole('checkbox', {
        name: "Also fill the channel's model list",
      }),
    )
    await goToConfirmAndSubmit(user)
    await user.click(
      await screen.findByRole('button', { name: 'Back to channel form' }),
    )

    expect(onApplyModels).not.toHaveBeenCalled()
  })
})

describe('ModelProfileWizard row confirmation and write-back', () => {
  it('never overwrites an existing profile unless the row opts in', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro'],
      entries: [entry({ id: 'gemini-2.5-pro' })],
    })
    mockCatalog([modelMeta({ id: 9, model_name: 'gemini-2.5-pro' })])

    renderWizard()

    await user.click(
      await screen.findByRole('checkbox', { name: 'gemini-2.5-pro' }),
    )
    await goToStep(user, 3)

    // 全部行都会被 skip：不能让人点「写入 0 个」。
    expect(screen.getByText('Create 0 / Update 0 / Skip 1')).toBeTruthy()
    expect(
      screen.getByText(
        'Nothing to write: existing profiles only update when you check Overwrite on their row.',
      ),
    ).toBeTruthy()
    const submit = screen.getByRole('button', {
      name: /^Write \d+ profiles$/,
    }) as HTMLButtonElement
    expect(submit.disabled).toBe(true)
    expect(mockPut).not.toHaveBeenCalled()
    expect(mockPost).not.toHaveBeenCalled()
  })

  it('counts only the rows that will actually be written', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['existing-model', 'new-model'],
      entries: [entry({ id: 'existing-model' }), entry({ id: 'new-model' })],
    })
    mockCatalog([modelMeta({ id: 9, model_name: 'existing-model' })])

    renderWizard()

    // 已有档案默认不勾选；新模型默认勾选。
    await user.click(
      await screen.findByRole('checkbox', { name: 'existing-model' }),
    )
    await goToStep(user, 3)

    expect(screen.getByText('Create 1 / Update 0 / Skip 1')).toBeTruthy()
    const submit = screen.getByRole('button', {
      name: 'Write 1 profiles',
    }) as HTMLButtonElement
    expect(submit.disabled).toBe(false)
  })

  it('updates the selected row and reports per-row results', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro'],
      entries: [entry({ id: 'gemini-2.5-pro' })],
    })
    mockCatalog([
      modelMeta({
        id: 9,
        model_name: 'gemini-2.5-pro',
        updated_at: '2026-10-20T08:00:00Z',
      }),
    ])

    renderWizard()

    await user.click(
      await screen.findByRole('checkbox', { name: 'gemini-2.5-pro' }),
    )
    await goToStep(user, 3)
    await user.click(screen.getByRole('checkbox', { name: 'Overwrite' }))
    await submitWizard(user)

    expect(await screen.findByText('Updated 1')).toBeTruthy()
    expect(mockPut).toHaveBeenCalledTimes(1)
    const [path, init] = mockPut.mock.calls[0] as unknown as [
      string,
      { params: { path: { id: number } }; body: { updated_at?: string } },
    ]
    expect(path).toBe('/api/models/{id}')
    expect(init.params.path.id).toBe(9)
    // 乐观锁：更新体必须带既有行的 updated_at。
    expect(init.body.updated_at).toBe('2026-10-20T08:00:00Z')
  })

  it('disables overwrite for locally locked profiles', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro'],
      entries: [entry({ id: 'gemini-2.5-pro' })],
    })
    mockCatalog([
      modelMeta({ id: 5, model_name: 'gemini-2.5-pro', sync_official: false }),
    ])

    renderWizard()

    await user.click(
      await screen.findByRole('checkbox', { name: 'gemini-2.5-pro' }),
    )
    await goToStep(user, 3)

    const overwrite = screen.getByRole('checkbox', {
      name: 'Overwrite',
    }) as HTMLInputElement
    expect(overwrite.disabled).toBe(true)
    expect(screen.getByText('Locked locally')).toBeTruthy()
    expect(screen.getByText(/maintained by hand/)).toBeTruthy()

    // 锁定行永远 skip，写入按钮随之禁用。
    expect(
      (screen.getByRole('button', {
        name: /^Write \d+ profiles$/,
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect(mockPut).not.toHaveBeenCalled()
    expect(mockPost).not.toHaveBeenCalled()
  })

  it('does not flag invalid input on a locked row it will never write', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro'],
      entries: [
        entry({ id: 'gemini-2.5-pro', description: 'x'.repeat(513) }),
      ],
    })
    mockCatalog([
      modelMeta({ id: 5, model_name: 'gemini-2.5-pro', sync_official: false }),
    ])

    renderWizard()

    await user.click(
      await screen.findByRole('checkbox', { name: 'gemini-2.5-pro' }),
    )
    await goToStep(user, 3)

    // 锁定行不可展开、永不写库；同一形状的未锁定行会渲染 role="img" 的
    // Description 错误标记，锁定行不应有。
    expect(screen.queryByRole('img', { name: 'Description' })).toBeNull()
    expect(screen.getByText('Locked locally')).toBeTruthy()
  })

  it('keeps going when one row fails', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['gpt-4o', 'gemini-2.5-pro'],
      entries: [entry({ id: 'gpt-4o' }), entry({ id: 'gemini-2.5-pro' })],
    })
    mockCatalog([modelMeta({ id: 9, model_name: 'gemini-2.5-pro' })])
    mockPut.mockRejectedValue(
      new ApiError({
        status: 409,
        type: 'https://newtg.dev/problems/conflict',
        title: 'Conflict',
        kind: 'conflict',
      }),
    )

    renderWizard()

    await screen.findByText('gpt-4o')
    await user.click(screen.getByRole('checkbox', { name: 'gemini-2.5-pro' }))
    await goToStep(user, 3)
    await user.click(screen.getByRole('checkbox', { name: 'Overwrite' }))
    await submitWizard(user)

    expect(await screen.findByText('Created 1')).toBeTruthy()
    expect(screen.getByText('Failed 1')).toBeTruthy()
    expect(
      screen.getByText(
        'This profile was changed by someone else. Refresh and try again.',
      ),
    ).toBeTruthy()
    // 第一行照常写库，第二行的 409 只影响它自己。
    expect(mockPost).toHaveBeenCalledTimes(1)
    expect(mockPut).toHaveBeenCalledTimes(1)
  })

  it('creates the explicitly named vendor at most once', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['alpha-model', 'beta-model'],
      entries: [entry({ id: 'alpha-model' }), entry({ id: 'beta-model' })],
    })
    mockCatalog([])
    mockPost.mockImplementation(((path: string) => {
      if (path === '/api/vendors') {
        return Promise.resolve({ data: { id: 7, name: 'OpenAI', status: 1 } })
      }
      return Promise.resolve({ data: {} })
    }) as never)

    renderWizard()

    await screen.findByText('alpha-model')
    await goToStep(user, 2)
    await user.selectOptions(screen.getByLabelText('Vendor'), CREATE_VENDOR)
    await user.type(screen.getByLabelText('New vendor name'), ' OpenAI ')
    await user.click(
      screen.getByRole('button', { name: 'Apply to all selected models' }),
    )
    await user.click(screen.getByRole('button', { name: 'Next' }))
    await submitWizard(user)

    expect(await screen.findByText('Created 2')).toBeTruthy()
    const calls = mockPost.mock.calls as unknown as Array<
      [string, { body?: { name?: string; model_name?: string; vendor_id?: number } }]
    >
    const vendorCalls = calls.filter(([path]) => path === '/api/vendors')
    expect(vendorCalls).toHaveLength(1)
    expect(vendorCalls[0]?.[1].body?.name).toBe('OpenAI')

    const modelCalls = calls.filter(([path]) => path === '/api/models')
    expect(modelCalls).toHaveLength(2)
    expect(modelCalls.map(([, init]) => init.body?.vendor_id)).toEqual([7, 7])
  })

  it('applies an explicitly chosen vendor even when only blank fields are filled', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['alpha-model'],
      entries: [entry({ id: 'alpha-model', vendor_suggestion: 'Gemini' })],
    })
    mockCatalog([])
    mockPost.mockImplementation(((path: string) => {
      if (path === '/api/vendors') {
        return Promise.resolve({ data: { id: 7, name: 'MyProxy', status: 1 } })
      }
      return Promise.resolve({ data: {} })
    }) as never)

    renderWizard()

    await screen.findByText('alpha-model')
    await goToStep(user, 2)
    await user.selectOptions(screen.getByLabelText('Vendor'), CREATE_VENDOR)
    await user.type(screen.getByLabelText('New vendor name'), 'MyProxy')
    // 默认勾着「只填空白字段」——正是它会让探测建议值挡住显式选择。
    expect(
      (
        screen.getByRole('checkbox', {
          name: 'Only fill blank fields',
        }) as HTMLInputElement
      ).checked,
    ).toBe(true)
    await user.click(
      screen.getByRole('button', { name: 'Apply to all selected models' }),
    )
    await user.click(screen.getByRole('button', { name: 'Next' }))
    await submitWizard(user)

    expect(await screen.findByText('Created 1')).toBeTruthy()
    const calls = mockPost.mock.calls as unknown as Array<
      [string, { body?: { name?: string; vendor_id?: number } }]
    >
    const vendorCalls = calls.filter(([path]) => path === '/api/vendors')
    expect(vendorCalls).toHaveLength(1)
    expect(vendorCalls[0]?.[1].body?.name).toBe('MyProxy')
    const modelCalls = calls.filter(([path]) => path === '/api/models')
    expect(modelCalls).toHaveLength(1)
    expect(modelCalls[0]?.[1].body?.vendor_id).toBe(7)
  })

  it('never creates a vendor from an upstream suggestion', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['alpha-model', 'beta-model'],
      entries: [
        entry({ id: 'alpha-model', vendor_suggestion: 'Gemini' }),
        entry({ id: 'beta-model', vendor_suggestion: 'Gemini' }),
      ],
    })
    mockCatalog([])

    renderWizard()

    await screen.findByText('alpha-model')
    await goToStep(user, 3)

    expect(
      screen.getAllByText(
        'Suggested vendor "Gemini" is not in the vendor library, so it will not be set.',
      ).length,
    ).toBeGreaterThan(0)

    await submitWizard(user)

    expect(await screen.findByText('Created 2')).toBeTruthy()
    const calls = mockPost.mock.calls as unknown as Array<
      [string, { body?: { vendor_id?: number | null } }]
    >
    expect(calls.filter(([path]) => path === '/api/vendors')).toHaveLength(0)
    const modelCalls = calls.filter(([path]) => path === '/api/models')
    expect(modelCalls).toHaveLength(2)
    expect(modelCalls.map(([, init]) => init.body?.vendor_id)).toEqual([
      null,
      null,
    ])
  })

  it('adds a capability to an existing profile instead of replacing it', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro'],
      entries: [entry({ id: 'gemini-2.5-pro' })],
    })
    mockCatalog([
      modelMeta({
        id: 9,
        model_name: 'gemini-2.5-pro',
        capabilities: ['text', 'vision'],
      }),
    ])

    renderWizard()

    await user.click(
      await screen.findByRole('checkbox', { name: 'gemini-2.5-pro' }),
    )
    await goToStep(user, 3)
    await user.click(screen.getByRole('checkbox', { name: 'Overwrite' }))
    await user.click(screen.getByRole('button', { name: 'gemini-2.5-pro' }))

    expect(
      (
        screen.getByRole('checkbox', {
          name: 'admin.models.capability_text',
        }) as HTMLInputElement
      ).checked,
    ).toBe(true)
    await user.click(
      screen.getByRole('checkbox', {
        name: 'admin.models.capability_reasoning',
      }),
    )
    await submitWizard(user)

    expect(await screen.findByText('Updated 1')).toBeTruthy()
    const [, init] = mockPut.mock.calls[0] as unknown as [
      string,
      { body: { capabilities?: string[] } },
    ]
    // 既有值是叠加的起点：勾一个只新增，不把 text / vision 丢掉。
    expect(init.body.capabilities).toEqual(['text', 'vision', 'reasoning'])
  })

  it('retries only the failed rows and updates the counts', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['gpt-4o', 'gemini-2.5-pro'],
      entries: [entry({ id: 'gpt-4o' }), entry({ id: 'gemini-2.5-pro' })],
    })
    mockCatalog([modelMeta({ id: 9, model_name: 'gemini-2.5-pro' })])
    mockPut.mockRejectedValueOnce(
      new ApiError({
        status: 409,
        type: 'https://newtg.dev/problems/conflict',
        title: 'Conflict',
        kind: 'conflict',
      }),
    )

    renderWizard()

    await screen.findByText('gpt-4o')
    await user.click(screen.getByRole('checkbox', { name: 'gemini-2.5-pro' }))
    await goToStep(user, 3)
    await user.click(screen.getByRole('checkbox', { name: 'Overwrite' }))
    await submitWizard(user)

    expect(await screen.findByText('Created 1')).toBeTruthy()
    expect(screen.getByText('Failed 1')).toBeTruthy()

    await user.click(
      screen.getByRole('button', { name: 'Retry failed rows only' }),
    )

    await waitFor(() => {
      expect(mockPut).toHaveBeenCalledTimes(2)
    })
    // 成功行不重发。
    expect(mockPost).toHaveBeenCalledTimes(1)
    expect(await screen.findByText('Updated 1')).toBeTruthy()
    expect(screen.queryByText('Failed 1')).toBeNull()
    expect(screen.getByText('Created 1')).toBeTruthy()
  })

  it('does not recreate the vendor when retrying after a failed write', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['alpha-model'],
      entries: [entry({ id: 'alpha-model' })],
    })
    mockCatalog([])
    let modelPosts = 0
    mockPost.mockImplementation(((path: string) => {
      if (path === '/api/vendors') {
        return Promise.resolve({ data: { id: 7, name: 'MyProxy', status: 1 } })
      }
      modelPosts += 1
      if (modelPosts === 1) {
        return Promise.reject(
          new ApiError({
            status: 500,
            type: 'https://newtg.dev/problems/server_error',
            title: 'Server error',
            kind: 'server',
          }),
        )
      }
      return Promise.resolve({ data: {} })
    }) as never)

    renderWizard()

    await screen.findByText('alpha-model')
    await goToStep(user, 2)
    await user.selectOptions(screen.getByLabelText('Vendor'), CREATE_VENDOR)
    await user.type(screen.getByLabelText('New vendor name'), 'MyProxy')
    await user.click(
      screen.getByRole('button', { name: 'Apply to all selected models' }),
    )
    await user.click(screen.getByRole('button', { name: 'Next' }))
    await submitWizard(user)

    expect(await screen.findByText('Failed 1')).toBeTruthy()
    const vendorCalls = (): Array<[string]> =>
      (mockPost.mock.calls as unknown as Array<[string]>).filter(
        ([path]) => path === '/api/vendors',
      )
    // 首轮建商一次。
    expect(vendorCalls()).toHaveLength(1)

    await user.click(
      screen.getByRole('button', { name: 'Retry failed rows only' }),
    )

    expect(await screen.findByText('Created 1')).toBeTruthy()
    // 目录刷新后仍可能没有这家新供应商；重试必须复用 createdVendorIdsRef，
    // 不再撞唯一索引。
    expect(vendorCalls()).toHaveLength(1)
  })

  it('reports an existing vendor instead of a profile conflict', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['alpha-model'],
      entries: [entry({ id: 'alpha-model', vendor_suggestion: 'Acme' })],
    })
    mockCatalog([])
    mockPost.mockImplementation(((path: string) => {
      if (path === '/api/vendors') {
        return Promise.reject(
          new ApiError({
            status: 409,
            type: 'https://newtg.dev/problems/conflict',
            title: 'Conflict',
            kind: 'conflict',
          }),
        )
      }
      return Promise.resolve({ data: {} })
    }) as never)

    renderWizard()

    await screen.findByText('alpha-model')
    await goToStep(user, 2)
    await user.selectOptions(screen.getByLabelText('Vendor'), CREATE_VENDOR)
    await user.type(screen.getByLabelText('New vendor name'), 'Acme')
    await user.click(screen.getByRole('button', { name: 'Next' }))
    await submitWizard(user)

    expect(await screen.findByText('Failed 1')).toBeTruthy()
    expect(
      screen.getByText('Vendor "Acme" already exists; refreshing the list.'),
    ).toBeTruthy()
    const calls = mockPost.mock.calls as unknown as Array<[string]>
    expect(calls.filter(([path]) => path === '/api/models')).toHaveLength(0)
  })

  it('fails only the rows that use a vendor that could not be created', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['alpha-model', 'beta-model'],
      entries: [
        entry({ id: 'alpha-model', vendor_suggestion: 'Acme' }),
        entry({ id: 'beta-model' }),
      ],
    })
    mockCatalog([])
    mockPost.mockImplementation(((path: string) => {
      if (path === '/api/vendors') {
        return Promise.reject(
          new ApiError({
            status: 500,
            type: 'https://newtg.dev/problems/server_error',
            title: 'Vendor error',
            kind: 'server',
          }),
        )
      }
      return Promise.resolve({ data: {} })
    }) as never)

    renderWizard()

    await screen.findByText('alpha-model')
    await goToStep(user, 2)
    // 显式新建名与 alpha 的建议值相同：只有这一行依赖建商。
    await user.selectOptions(screen.getByLabelText('Vendor'), CREATE_VENDOR)
    await user.type(screen.getByLabelText('New vendor name'), 'Acme')
    await user.click(screen.getByRole('button', { name: 'Next' }))
    await submitWizard(user)

    expect(await screen.findByText('Created 1')).toBeTruthy()
    expect(screen.getByText('Failed 1')).toBeTruthy()
    expect(
      screen.getByText('Could not create vendor "Acme": Vendor error'),
    ).toBeTruthy()

    const calls = mockPost.mock.calls as unknown as Array<
      [string, { body?: { model_name?: string } }]
    >
    expect(calls.filter(([path]) => path === '/api/vendors')).toHaveLength(1)
    const modelCalls = calls.filter(([path]) => path === '/api/models')
    expect(modelCalls).toHaveLength(1)
    expect(modelCalls[0]?.[1].body?.model_name).toBe('beta-model')
  })

  it('allows exactly MAX_WIZARD_ROWS rows but blocks one more', async () => {
    const user = userEvent.setup()
    const entries = Array.from({ length: 200 }, (_, index) =>
      entry({ id: `model-${index}` }),
    )
    mockFetch.mockResolvedValue({
      models: entries.map((item) => item.id),
      entries,
    })
    mockCatalog([])

    renderWizard()

    await screen.findByText('model-0')
    await goToStep(user, 3)

    expect(
      screen.queryByText(
        'Confirm at most 200 models at once. Narrow the selection first.',
      ),
    ).toBeNull()
    const submitButton = screen.getByRole('button', {
      name: /^Write \d+ profiles$/,
    }) as HTMLButtonElement
    expect(submitButton.disabled).toBe(false)
    expect(mockPost).not.toHaveBeenCalled()
  })

  it('flags an invalid inline value and skips only that row', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['alpha-model', 'beta-model'],
      entries: [entry({ id: 'alpha-model' }), entry({ id: 'beta-model' })],
    })
    mockCatalog([])

    renderWizard()

    await screen.findByText('alpha-model')
    await goToStep(user, 3)

    await user.click(screen.getByRole('button', { name: 'beta-model' }))
    const context = screen.getByLabelText('Context')
    await user.clear(context)
    await user.type(context, 'abc')
    expect(screen.getByText('Enter a non-negative integer.')).toBeTruthy()

    await submitWizard(user)

    expect(await screen.findByText('Created 1')).toBeTruthy()
    expect(screen.getByText('Failed 1')).toBeTruthy()
    expect(
      screen.getByText('This row has uncorrected input.'),
    ).toBeTruthy()

    const calls = mockPost.mock.calls as unknown as Array<
      [string, { body?: { model_name?: string } }]
    >
    const modelCalls = calls.filter(([path]) => path === '/api/models')
    expect(modelCalls).toHaveLength(1)
    expect(modelCalls[0]?.[1].body?.model_name).toBe('alpha-model')
  })

  it('blocks submission when more than MAX_WIZARD_ROWS are selected', async () => {
    const user = userEvent.setup()
    // 用真实目录返回 201 条，端到端覆盖「阻塞提示 + 提交按钮不可用」；
    // 只单测常量判定证明不了 UI 真的拦住了写库。
    const entries = Array.from({ length: 201 }, (_, index) =>
      entry({ id: `model-${index}` }),
    )
    mockFetch.mockResolvedValue({
      models: entries.map((item) => item.id),
      entries,
    })
    mockCatalog([])

    renderWizard()

    await screen.findByText('model-0')
    await goToStep(user, 3)

    expect(
      screen.getByText(
        'Confirm at most 200 models at once. Narrow the selection first.',
      ),
    ).toBeTruthy()
    const submitButton = screen.getByRole('button', {
      name: /^Write \d+ profiles$/,
    }) as HTMLButtonElement
    expect(submitButton.disabled).toBe(true)
    expect(mockPost).not.toHaveBeenCalled()
  })

  it('reports a create conflict as an existing profile and does not retry it', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['alpha-model'],
      entries: [entry({ id: 'alpha-model' })],
    })
    mockCatalog([])
    mockPost.mockRejectedValue(
      new ApiError({
        status: 409,
        type: 'https://newtg.dev/problems/conflict',
        title: 'Conflict',
        kind: 'conflict',
      }),
    )

    renderWizard()

    await screen.findByText('alpha-model')
    await goToConfirmAndSubmit(user)

    expect(await screen.findByText('Failed 1')).toBeTruthy()
    // 创建路径的 409 是「同名档案已存在」，不是「被他人修改」。
    expect(
      screen.getByText(/A profile with this name already exists/),
    ).toBeTruthy()
    expect(
      (screen.getByRole('button', {
        name: 'Retry failed rows only',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
  })

  it('does not offer retry for a deterministic 422 failure', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['alpha-model'],
      entries: [entry({ id: 'alpha-model' })],
    })
    mockCatalog([])
    mockPost.mockRejectedValue(
      new ApiError({
        status: 422,
        type: 'https://newtg.dev/problems/validation',
        title: 'Unprocessable entity',
        kind: 'validation',
      }),
    )

    renderWizard()

    await screen.findByText('alpha-model')
    await goToConfirmAndSubmit(user)

    expect(await screen.findByText('Failed 1')).toBeTruthy()
    expect(
      (screen.getByRole('button', {
        name: 'Retry failed rows only',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
  })

  it('flags an invalid upstream model name on step 1 and never writes it', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro@vertex'],
      entries: [entry({ id: 'gemini-2.5-pro@vertex' })],
    })
    mockCatalog([])

    renderWizard()

    await screen.findByText('gemini-2.5-pro@vertex')
    expect(
      screen.getByRole('img', {
        name: 'Invalid model name; this row cannot be created (only letters, digits and . _ : / - are allowed).',
      }),
    ).toBeTruthy()

    await goToConfirmAndSubmit(user)

    expect(await screen.findByText('Failed 1')).toBeTruthy()
    expect(screen.getByText('This row has uncorrected input.')).toBeTruthy()
    expect(mockPost).not.toHaveBeenCalled()
    expect(
      (screen.getByRole('button', {
        name: 'Retry failed rows only',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
  })

  it('warns about a truncated catalog and does not preselect unconfirmed rows', async () => {
    mockFetch.mockResolvedValue({
      models: ['alpha-model'],
      entries: [entry({ id: 'alpha-model' })],
    })
    mockGet.mockImplementation(((path: string) =>
      path === '/api/models'
        ? Promise.resolve({ data: { items: [], total: 1001 } })
        : Promise.resolve(listResponse([]))) as never)

    renderWizard()

    expect(
      await screen.findByText(/more than 1000 entries/),
    ).toBeTruthy()
    const checkbox = screen.getByRole('checkbox', {
      name: 'alpha-model',
    }) as HTMLInputElement
    // 目录没加载全时「查不到档案」不等于「没有档案」，不能默认勾选。
    expect(checkbox.checked).toBe(false)
  })

  it('shows the resolved local vendor in the collapsed summary', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['alpha-model'],
      entries: [entry({ id: 'alpha-model', vendor_suggestion: 'Acme' })],
    })
    mockCatalog([], [{ id: 3, name: 'Acme' }])

    renderWizard()

    await screen.findByText('alpha-model')
    await goToStep(user, 3)

    expect(await screen.findByText('Vendor: Acme')).toBeTruthy()
  })

  it('shows the probe facts source in the expanded row', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro'],
      entries: [
        entry({ id: 'gemini-2.5-pro', facts_source: 'gemini_models' }),
      ],
    })
    mockCatalog([])

    renderWizard()

    await screen.findByText('gemini-2.5-pro')
    await goToStep(user, 3)
    await user.click(screen.getByRole('button', { name: 'gemini-2.5-pro' }))

    expect(screen.getByText('Facts source: gemini_models')).toBeTruthy()
  })

  it('keeps applied defaults when a catalog refetch fails after a success', async () => {
    const user = userEvent.setup()
    mockFetch.mockResolvedValue({
      models: ['gemini-2.5-pro'],
      entries: [entry({ id: 'gemini-2.5-pro' })],
    })
    let catalogCalls = 0
    mockGet.mockImplementation(((path: string) => {
      if (path !== '/api/models') return Promise.resolve(listResponse([]))
      catalogCalls += 1
      return catalogCalls === 1
        ? Promise.resolve(listResponse([]))
        : Promise.reject(new Error('catalog down'))
    }) as never)

    const { queryClient } = renderWizard()

    await screen.findByText('gemini-2.5-pro')
    await goToStep(user, 2)
    fireEvent.change(screen.getByLabelText('Knowledge cutoff'), {
      target: { value: '2025-06' },
    })
    await user.click(
      screen.getByRole('button', { name: 'Apply to all selected models' }),
    )

    // 目录先成功（已建行、已套默认值），再让后台重取失败。
    await queryClient.invalidateQueries()
    await waitFor(() => {
      expect(catalogCalls).toBeGreaterThan(1)
    })

    await user.click(screen.getByRole('button', { name: 'Next' }))
    await user.click(screen.getByRole('button', { name: 'gemini-2.5-pro' }))

    const cutoff = screen.getByLabelText('Knowledge cutoff') as HTMLInputElement
    expect(cutoff.value).toBe('2025-06')
  })
})
