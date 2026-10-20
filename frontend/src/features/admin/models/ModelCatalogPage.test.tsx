import { useState } from 'react'
import { HttpResponse, delay, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'
import { useToastStore } from '@/shared/ui/toast'

import { ModelCatalogPage } from './ModelCatalogPage'
import {
  DEFAULT_MODEL_CATALOG_SEARCH,
  type ModelCatalogSearch,
} from './model-catalog-search'

type ModelMeta = components['schemas']['ModelMeta']

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())
beforeEach(() => {
  useToastStore.setState({ toasts: [] })
  installHandlers()
})

function modelRow(overrides: Partial<ModelMeta> = {}): ModelMeta {
  return {
    id: 1,
    model_name: 'openai/gpt-5',
    display_name: 'GPT-5',
    description: null,
    icon: null,
    tags: ['flagship', 'vision', 'tools'],
    vendor_id: 7,
    endpoints: { '/v1/chat/completions': ['POST'] },
    capabilities: ['text', 'vision'],
    modalities: ['text', 'image'],
    parameters: [],
    context_length: 128000,
    max_output: 16384,
    knowledge_cutoff: '2025-06',
    released_at: '2026-01-01',
    status: 1,
    sync_official: true,
    name_rule: 0,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    vendor_name: 'OpenAI',
    square_state: 'visible',
    configured_channel_count: 3,
    enable_groups: ['default'],
    ...overrides,
  }
}

type Permissions = string[]
const FULL: Permissions = [
  'channels:read',
  'channels:manage',
  'channels:manage_sensitive',
  'pricing:manage',
]

interface InstallOptions {
  permissions?: Permissions
  items?: ModelMeta[]
  total?: number
  missing?: { model_name: string; channel_count: number; has_pricing: boolean }[]
  createStatus?: number
  createFields?: string[]
  updateStatus?: number
  batchStatus?: number
  batchChannelIds?: number[]
  syncPreviewStatus?: number
  syncApplyStatus?: number
  syncPreviewDelayMs?: number
  recordSyncPreview?: () => void
  recordList?: (params: URLSearchParams) => void
}

function installHandlers({
  permissions = FULL,
  items = [modelRow()],
  total = 1,
  missing = [],
  createStatus = 201,
  createFields,
  updateStatus = 200,
  batchStatus = 200,
  batchChannelIds,
  syncPreviewStatus = 200,
  syncApplyStatus = 200,
  syncPreviewDelayMs = 0,
  recordSyncPreview,
  recordList,
}: InstallOptions = {}) {
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({ username: 'alice', role: 'admin', status: 'enabled', permissions }),
    ),
    http.get(`${ORIGIN}/api/models`, ({ request }) => {
      const params = new URL(request.url).searchParams
      recordList?.(params)
      return HttpResponse.json({
        items,
        total,
        page: Number(params.get('p') ?? '1'),
        page_size: Number(params.get('page_size') ?? '20'),
      })
    }),
    http.get(`${ORIGIN}/api/models/missing`, () =>
      HttpResponse.json({ items: missing, skipped_channels: 0, total: missing.length }),
    ),
    http.get(`${ORIGIN}/api/vendors/search`, () =>
      HttpResponse.json({
        items: [
          { id: 7, name: 'OpenAI', status: 1 },
          { id: 8, name: 'Anthropic', status: 1 },
        ],
      }),
    ),
    http.get(`${ORIGIN}/api/vendors`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
    http.post(`${ORIGIN}/api/models`, () => {
      if (createStatus !== 200 && createStatus !== 201) {
        return HttpResponse.json(
          {
            type: 'about:blank',
            title: 'Validation failed',
            detail: 'invalid',
            fields: createFields ?? ['model_name'],
          },
          { status: createStatus, headers: { 'Content-Type': 'application/problem+json' } },
        )
      }
      return HttpResponse.json(modelRow({ id: 99, model_name: 'new/model' }), { status: 201 })
    }),
    http.put(`${ORIGIN}/api/models/:id`, () => {
      if (updateStatus !== 200) {
        return HttpResponse.json(
          { type: 'about:blank', title: 'Conflict', detail: 'stale' },
          { status: updateStatus, headers: { 'Content-Type': 'application/problem+json' } },
        )
      }
      return HttpResponse.json(modelRow())
    }),
    http.delete(`${ORIGIN}/api/models/:id`, () => new HttpResponse(null, { status: 204 })),
    http.post(`${ORIGIN}/api/models/batch-delete`, () => {
      if (batchStatus !== 200) {
        const body: Record<string, unknown> = {
          type: 'https://newtg.dev/problems/channel_models_empty',
          title: 'Channel models cannot be emptied',
          detail: 'removing these models would leave the listed channels empty',
        }
        if (batchChannelIds !== undefined) body.channel_ids = batchChannelIds
        return HttpResponse.json(body, {
          status: batchStatus,
          headers: { 'Content-Type': 'application/problem+json' },
        })
      }
      return HttpResponse.json({
        requested: 1,
        deleted: 1,
        channels_updated: 0,
        pricing_deleted: 0,
        group_pricing_deleted: 0,
      })
    }),
    http.get(`${ORIGIN}/api/models/sync/preview`, async () => {
      recordSyncPreview?.()
      if (syncPreviewDelayMs > 0) await delay(syncPreviewDelayMs)
      if (syncPreviewStatus !== 200) {
        return HttpResponse.json(
          { type: 'about:blank', title: 'Unavailable', detail: 'unconfigured' },
          { status: syncPreviewStatus, headers: { 'Content-Type': 'application/problem+json' } },
        )
      }
      return HttpResponse.json({
        source_version: 'abcdef0123456789abcdef0123456789',
        groups: {
          create: [{ model_name: 'new/model' }],
          update: [
            {
              model_name: 'openai/gpt-5',
              changes: { description: { local: null, upstream: 'Flagship' } },
            },
          ],
          unchanged: [],
          blocked: ['locked/model'],
          missing_upstream: [],
          missing_vendor: [],
          vendors: { create: ['NewVendor'], update: [], unchanged: [] },
        },
      })
    }),
    http.post(`${ORIGIN}/api/models/sync`, async () => {
      if (syncApplyStatus === 409) {
        return HttpResponse.json(
          { type: 'about:blank', title: 'Conflict', detail: 'version expired' },
          { status: 409, headers: { 'Content-Type': 'application/problem+json' } },
        )
      }
      return HttpResponse.json({
        source_version: 'abcdef0123456789abcdef0123456789',
        created_models: ['new/model'],
        updated_models: ['openai/gpt-5'],
        created_vendors: ['NewVendor'],
        updated_vendors: [],
      })
    }),
  )
}

function Harness({ initial }: { initial?: Partial<ModelCatalogSearch> }) {
  const [search, setSearch] = useState<ModelCatalogSearch>({
    ...DEFAULT_MODEL_CATALOG_SEARCH,
    ...initial,
  })
  return (
    <ModelCatalogPage
      search={search}
      onSearchChange={(next) => {
        setSearch(next)
      }}
    />
  )
}

describe('models tab', () => {
  it('renders the model row columns and the missing badge', async () => {
    installHandlers({
      missing: [{ model_name: 'orphan/model', channel_count: 2, has_pricing: false }],
    })
    await renderWithProviders(<Harness />, ['common', 'admin'])

    expect(await screen.findByText('openai/gpt-5')).toBeTruthy()
    expect(screen.getByText('GPT-5')).toBeTruthy()
    expect(screen.getAllByText('OpenAI').length).toBeGreaterThan(0)
    expect(screen.getByText('+1')).toBeTruthy()
    expect(screen.getAllByText('Listed').length).toBeGreaterThan(0)
    expect(screen.getByText('3')).toBeTruthy()
    expect(await screen.findByRole('button', { name: /Missing models/ })).toHaveProperty(
      'textContent',
      expect.stringContaining('1'),
    )
  })

  it('pushes filters into the URL callback and resets the page', async () => {
    const onSearchChange = vi.fn()
    const search: ModelCatalogSearch = { ...DEFAULT_MODEL_CATALOG_SEARCH, mv_p: 2 }
    await renderWithProviders(
      <ModelCatalogPage search={search} onSearchChange={onSearchChange} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.type(screen.getByLabelText('Keyword'), ' gpt ')
    await user.selectOptions(screen.getByLabelText('Status'), 'disabled')
    await user.click(screen.getByRole('button', { name: 'Search' }))

    expect(onSearchChange).toHaveBeenCalledWith(
      expect.objectContaining({ mv_p: 1, mv_keyword: 'gpt', mv_status: 'disabled' }),
      { replace: true },
    )
  })

  it('switches tabs without dropping each side’s filters', async () => {
    await renderWithProviders(
      <Harness initial={{ tab: 'models', mv_keyword: 'gpt', vv_keyword: 'openai' }} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('tab', { name: 'Vendors' }))
    expect(await screen.findByText('No vendors match these filters.')).toBeTruthy()
    await user.click(screen.getByRole('tab', { name: 'Models' }))
    expect(await screen.findByText('openai/gpt-5')).toBeTruthy()
    expect((screen.getByLabelText('Keyword') as HTMLInputElement).value).toBe('gpt')
  })

  it('advances the offset page through the URL callback', async () => {
    const onSearchChange = vi.fn()
    installHandlers({ total: 45 })
    await renderWithProviders(
      <ModelCatalogPage
        search={{ ...DEFAULT_MODEL_CATALOG_SEARCH, mv_p: 1 }}
        onSearchChange={onSearchChange}
      />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('button', { name: 'Next' }))
    expect(onSearchChange).toHaveBeenCalledWith(
      expect.objectContaining({ mv_p: 2 }),
      { replace: true },
    )
  })

  it('shows an empty state when the catalog has no models', async () => {
    installHandlers({ items: [], total: 0 })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    expect(await screen.findByText('No model metadata yet.')).toBeTruthy()
  })

  it('maps a 422 onto the offending field and keeps the sheet open', async () => {
    installHandlers({ createStatus: 422, createFields: ['vendor_id'] })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('button', { name: 'New model' }))
    const sheet = await screen.findByRole('dialog', { name: 'New model' })
    await user.type(within(sheet).getByLabelText('Model name'), 'new/model')
    await user.click(within(sheet).getByRole('button', { name: 'Save' }))

    expect(
      await within(sheet).findByText('The selected vendor does not exist.'),
    ).toBeTruthy()
    expect(screen.queryByRole('dialog', { name: 'New model' })).toBeTruthy()
  })

  it('reports a 409 conflict without retrying and refreshes the list', async () => {
    let listCalls = 0
    installHandlers({ updateStatus: 409, recordList: () => (listCalls += 1) })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')
    const before = listCalls

    await user.click(screen.getByRole('button', { name: 'Edit' }))
    const sheet = await screen.findByRole('dialog', { name: 'Edit model' })
    expect((within(sheet).getByLabelText('Model name') as HTMLInputElement).disabled).toBe(true)
    await user.click(within(sheet).getByRole('button', { name: 'Save' }))

    expect(await within(sheet).findByText(/changed elsewhere/)).toBeTruthy()
    await waitFor(() => {
      expect(listCalls).toBeGreaterThan(before)
    })
  })

  it('maps a create-time 409 to the duplicate-name field instead of the concurrency banner', async () => {
    installHandlers({ createStatus: 409, createFields: ['model_name'] })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('button', { name: 'New model' }))
    const sheet = await screen.findByRole('dialog', { name: 'New model' })
    await user.type(within(sheet).getByLabelText('Model name'), 'openai/gpt-5')
    await user.click(within(sheet).getByRole('button', { name: 'Save' }))

    expect(
      await within(sheet).findByText('A model with this name already exists.'),
    ).toBeTruthy()
    expect(within(sheet).queryByText(/changed elsewhere/)).toBeNull()
  })

  it('renders a server display_name 422 inline and summarises unrendered fields', async () => {
    installHandlers({ createStatus: 422, createFields: ['display_name', 'parameters'] })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('button', { name: 'New model' }))
    const sheet = await screen.findByRole('dialog', { name: 'New model' })
    await user.type(within(sheet).getByLabelText('Model name'), 'new/model')
    await user.click(within(sheet).getByRole('button', { name: 'Save' }))

    expect(
      await within(sheet).findByText('Display name must be at most 64 characters.'),
    ).toBeTruthy()
    // parameters 没有行内输入位：必须汇总到表单级，否则 422 不可见。
    expect(
      within(sheet).getByText('Some fields were rejected: parameters.'),
    ).toBeTruthy()
  })

  it('rejects a 65-rune display_name before submitting', async () => {
    installHandlers({
      items: [modelRow({ display_name: 'x'.repeat(65) })],
    })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('button', { name: 'Edit' }))
    const sheet = await screen.findByRole('dialog', { name: 'Edit model' })
    await user.click(within(sheet).getByRole('button', { name: 'Save' }))

    expect(
      await within(sheet).findByText('Display name must be at most 64 characters.'),
    ).toBeTruthy()
  })

  it('rejects duplicate endpoint paths before submitting', async () => {
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('button', { name: 'Edit' }))
    const sheet = await screen.findByRole('dialog', { name: 'Edit model' })
    await user.click(within(sheet).getByRole('button', { name: 'Add endpoint' }))

    const paths = within(sheet).getAllByLabelText('Path')
    expect(paths).toHaveLength(2)
    const duplicateRow = paths[1]
    if (duplicateRow === undefined) throw new Error('second endpoint row missing')
    await user.type(duplicateRow, '/v1/chat/completions')
    await user.click(within(sheet).getByRole('button', { name: 'Save' }))

    expect(
      await within(sheet).findByText(
        'Endpoint path /v1/chat/completions appears more than once.',
      ),
    ).toBeTruthy()
  })

  it('rejects a tag longer than 32 runes even when it uses multibyte characters', async () => {
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('button', { name: 'Edit' }))
    const sheet = await screen.findByRole('dialog', { name: 'Edit model' })
    // 33 runes、66 UTF-8 bytes：服务端按 rune 校验，前端必须同口径。
    fireEvent.change(within(sheet).getByLabelText('Tags'), {
      target: { value: 'é'.repeat(33) },
    })
    await user.click(within(sheet).getByRole('button', { name: 'Add' }))

    expect(
      await within(sheet).findByText('Each tag must be 1–32 characters.'),
    ).toBeTruthy()
    // 被拒绝的标签不得进入选择集。
    expect(within(sheet).queryByText('é'.repeat(33))).toBeNull()
  })

  it('lists the channel ids when batch delete is rejected with channel_ids', async () => {
    installHandlers({ batchStatus: 422, batchChannelIds: [12, 19] })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('checkbox', { name: 'Select all models on this page' }))
    await user.click(screen.getByRole('button', { name: /Delete selected/ }))
    const sheet = await screen.findByRole('dialog', { name: /Delete 1 models/ })
    await user.click(within(sheet).getByRole('button', { name: 'Delete (1)' }))

    expect(
      await within(sheet).findByText(
        'Cannot delete: channels #12, #19 would be left without any supported model.',
      ),
    ).toBeTruthy()
  })

  it('disables “remove from channels” for non-exact rules and locks clear pricing without permission', async () => {
    installHandlers({
      permissions: ['channels:read', 'channels:manage', 'channels:manage_sensitive'],
      items: [modelRow({ id: 1, name_rule: 1 }), modelRow({ id: 2, model_name: 'other/model', name_rule: 0 })],
      total: 2,
    })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('checkbox', { name: 'Select all models on this page' }))
    await user.click(screen.getByRole('button', { name: /Delete selected/ }))
    const sheet = await screen.findByRole('dialog', { name: /Delete 2 models/ })

    expect(
      (within(sheet).getByRole('checkbox', { name: /Also remove from channels/ }) as HTMLInputElement)
        .disabled,
    ).toBe(true)
    expect(
      (within(sheet).getByRole('checkbox', { name: /Also delete pricing/ }) as HTMLInputElement)
        .disabled,
    ).toBe(true)
    expect(within(sheet).getByText(/Requires the pricing:manage permission/)).toBeTruthy()
  })

  it('prefills the create sheet from the missing models drawer', async () => {
    installHandlers({
      missing: [{ model_name: 'orphan/model', channel_count: 2, has_pricing: true }],
    })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('button', { name: /Missing models/ }))
    const drawer = await screen.findByRole('dialog', { name: 'Missing models' })
    await user.click(await within(drawer).findByRole('button', { name: 'Create record' }))

    const sheet = await screen.findByRole('dialog', { name: 'New model' })
    expect((within(sheet).getByLabelText('Model name') as HTMLInputElement).value).toBe(
      'orphan/model',
    )
  })

  it('never offers write entry points without channels:manage', async () => {
    installHandlers({ permissions: ['channels:read'] })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    await screen.findByText('openai/gpt-5')

    expect(screen.queryByRole('button', { name: 'New model' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Sync from upstream' })).toBeNull()
    expect(screen.queryByRole('button', { name: /Delete selected/ })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Edit' })).toBeNull()
  })

  it('previews, applies only the selected rows and requires a re-preview on 409', async () => {
    let applyBody: unknown
    server.use(
      http.post(`${ORIGIN}/api/models/sync`, async ({ request }) => {
        applyBody = await request.json()
        return HttpResponse.json(
          { type: 'about:blank', title: 'Conflict', detail: 'expired' },
          { status: 409, headers: { 'Content-Type': 'application/problem+json' } },
        )
      }),
    )
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('button', { name: 'Sync from upstream' }))
    const dialog = await screen.findByRole('dialog', { name: 'Sync from upstream' })
    expect(await within(dialog).findByText('new/model')).toBeTruthy()
    expect(within(dialog).getByText('locked/model')).toBeTruthy()

    // blocked 条目不可勾选。
    const blocked = within(dialog).getByRole('checkbox', { name: /locked\/model/ }) as HTMLInputElement
    expect(blocked.disabled).toBe(true)

    await user.click(within(dialog).getByRole('button', { name: /Apply selected/ }))

    await waitFor(() => {
      expect(applyBody).toEqual({
        source_version: 'abcdef0123456789abcdef0123456789',
        models: [{ model_name: 'new/model' }, { model_name: 'openai/gpt-5' }],
        vendors: [{ name: 'NewVendor' }],
      })
    })
    expect(await within(dialog).findByText(/Preview again before applying/)).toBeTruthy()
    expect(within(dialog).getByRole('button', { name: 'Preview again' })).toBeTruthy()
  })

  it('clears the 409 conflict after Preview again even when the new preview is identical', async () => {
    // 重新预览返回的清单与旧清单结构完全相同：React Query 的结构共享会保留
    // 同一个 `data` 引用。冲突标记必须按「请求成功」事件重置，而不是按引用变化，
    // 否则 Apply 会永远禁用、管理员无法重试。
    let previewCalls = 0
    installHandlers({
      syncApplyStatus: 409,
      recordSyncPreview: () => {
        previewCalls += 1
      },
    })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('button', { name: 'Sync from upstream' }))
    const dialog = await screen.findByRole('dialog', { name: 'Sync from upstream' })
    await within(dialog).findByText('new/model')
    expect(previewCalls).toBe(1)

    await user.click(within(dialog).getByRole('button', { name: /Apply selected/ }))
    expect(await within(dialog).findByText(/Preview again before applying/)).toBeTruthy()
    const applyButton = () =>
      within(dialog).getByRole('button', { name: /Apply selected/ }) as HTMLButtonElement
    expect(applyButton().disabled).toBe(true)

    await user.click(within(dialog).getByRole('button', { name: 'Preview again' }))
    await waitFor(() => {
      expect(previewCalls).toBe(2)
    })

    // 同样的清单落地后，冲突提示消失、Apply 恢复可用。
    await waitFor(() => {
      expect(within(dialog).queryByText(/Preview again before applying/)).toBeNull()
    })
    expect(applyButton().disabled).toBe(false)
  })

  it('discards the cached preview when the dialog closes', async () => {
    let previewCalls = 0
    installHandlers({ recordSyncPreview: () => { previewCalls += 1 } })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('button', { name: 'Sync from upstream' }))
    const dialog = await screen.findByRole('dialog', { name: 'Sync from upstream' })
    await within(dialog).findByText('new/model')
    expect(previewCalls).toBe(1)

    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    await waitFor(() => {
      expect(screen.queryByRole('dialog', { name: 'Sync from upstream' })).toBeNull()
    })

    // 重新打开必须重新预览：若缓存未清除，这里会渲染旧清单且不发请求。
    await user.click(screen.getByRole('button', { name: 'Sync from upstream' }))
    const reopened = await screen.findByRole('dialog', { name: 'Sync from upstream' })
    await within(reopened).findByText('new/model')
    expect(previewCalls).toBe(2)
  })

  it('disables Apply and Preview again while the preview is refetching', async () => {
    installHandlers({ syncApplyStatus: 409, syncPreviewDelayMs: 250 })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('button', { name: 'Sync from upstream' }))
    const dialog = await screen.findByRole('dialog', { name: 'Sync from upstream' })
    await within(dialog).findByText('new/model')

    await user.click(within(dialog).getByRole('button', { name: /Apply selected/ }))
    const repreview = await within(dialog).findByRole('button', { name: 'Preview again' })

    await user.click(repreview)
    // 预览尚在飞行中：两个动作都必须锁定，避免用半旧的清单继续操作。
    expect((repreview as HTMLButtonElement).disabled).toBe(true)
    expect(
      (within(dialog).getByRole('button', { name: /Apply selected/ }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)

    // 新预览落地后解除 isFetching 锁定。
    await waitFor(() => {
      expect((repreview as HTMLButtonElement).disabled).toBe(false)
    })
  })

  it('explains a 503 preview when the source URL is not configured', async () => {
    installHandlers({ syncPreviewStatus: 503 })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('openai/gpt-5')

    await user.click(screen.getByRole('button', { name: 'Sync from upstream' }))
    const dialog = await screen.findByRole('dialog', { name: 'Sync from upstream' })
    expect(await within(dialog).findByText(/model_sync.source_url is not configured/)).toBeTruthy()
  })
})
