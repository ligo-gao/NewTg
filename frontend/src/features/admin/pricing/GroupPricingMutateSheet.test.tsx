import { useState } from 'react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'
import { useToastStore } from '@/shared/ui/toast'

import { GroupPricingMutateSheet } from './GroupPricingMutateSheet'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())
beforeEach(() => {
  useToastStore.setState({ toasts: [] })
})

function installHandlers() {
  server.use(
    http.get(`${ORIGIN}/api/groups`, () =>
      HttpResponse.json({
        items: [
          {
            id: 1,
            name: 'vip',
            description: null,
            ratio: 120,
            user_selectable: true,
            is_system: false,
            status: 1,
            users_count: 0,
            pricing_overrides_count: 0,
            channels_count: 0,
            created_at: '2026-10-01T10:00:00Z',
            updated_at: '2026-10-01T10:00:00Z',
          },
        ],
        total: 1,
        page: 1,
        page_size: 100,
      }),
    ),
    http.get(`${ORIGIN}/api/model-pricing/models`, () =>
      HttpResponse.json({
        items: [
          { model: 'gpt-5', mode: 'per_token', status: 1 },
          { model: 'tts-1', mode: 'per_request', status: 1 },
          { model: 'expr-model', mode: 'tiered_expr', status: 1 },
        ],
      }),
    ),
  )
}

async function renderSheet() {
  return renderWithProviders(
    <GroupPricingMutateSheet
      open
      mode="create"
      quotaPerUnit={100}
      onOpenChange={() => undefined}
      onSaved={() => undefined}
    />,
    ['common', 'admin'],
  )
}

function overrideRow(overrides: Record<string, unknown> = {}) {
  return {
    id: 1,
    group: 'vip',
    model: 'gpt-5',
    mode: 'per_token',
    prompt_price: 400,
    completion_price: 1200,
    cache_read_price: 40,
    cache_write_price: 120,
    status: 1,
    source_status: 1,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    ...overrides,
  } as components['schemas']['GroupPricing']
}

async function renderEdit(override: components['schemas']['GroupPricing']) {
  return renderWithProviders(
    <GroupPricingMutateSheet
      open
      mode="edit"
      override={override}
      quotaPerUnit={100}
      onOpenChange={() => undefined}
      onSaved={() => undefined}
    />,
    ['common', 'admin'],
  )
}

/**
 * 受控外壳：Escape 关闭后真正卸载，用于验证「只关栈顶模态」。测试里若
 * open 恒为 true，onOpenChange 不会生效，就无法断言 Sheet 是否被关闭。
 */
function ControlledSheet() {
  const [open, setOpen] = useState(true)
  return (
    <GroupPricingMutateSheet
      open={open}
      mode="create"
      quotaPerUnit={100}
      onOpenChange={setOpen}
      onSaved={() => undefined}
    />
  )
}

describe('GroupPricingMutateSheet', () => {
  it('filters expression-priced sources out of the model selector', async () => {
    installHandlers()

    const { findByRole, queryByRole } = await renderSheet()

    expect(await findByRole('option', { name: 'gpt-5' })).toBeTruthy()
    expect(await findByRole('option', { name: 'tts-1' })).toBeTruthy()
    expect(queryByRole('option', { name: 'expr-model' })).toBeNull()
  })

  it('pins hidden dimensions to zero for a per-request source', async () => {
    installHandlers()
    const bodies: Record<string, unknown>[] = []
    server.use(
      http.post(`${ORIGIN}/api/group-pricing`, async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>)
        return HttpResponse.json(
          {
            id: 1,
            group: 'vip',
            model: 'tts-1',
            mode: 'per_request',
            prompt_price: 200,
            completion_price: 0,
            cache_read_price: 0,
            cache_write_price: 0,
            status: 1,
            source_status: 1,
            created_at: '2026-10-01T10:00:00Z',
            updated_at: '2026-10-01T10:00:00Z',
          },
          { status: 201 },
        )
      }),
    )

    const { findByRole, getByLabelText, getByRole, queryByLabelText } =
      await renderSheet()
    const user = userEvent.setup()

    await findByRole('option', { name: 'vip' })
    await user.selectOptions(getByLabelText('Group'), 'vip')
    await findByRole('option', { name: 'tts-1' })
    await user.selectOptions(getByLabelText('Model'), 'tts-1')

    // 按次源模型每次请求只计一个价；其他维度根本不渲染（UI §8.3）。
    expect(queryByLabelText('Output price')).toBeNull()
    await user.type(getByLabelText('Price per request'), '200')
    await user.click(getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(bodies).toHaveLength(1)
    })
    expect(bodies[0]).toMatchObject({
      group: 'vip',
      model: 'tts-1',
      prompt_price: 200,
      completion_price: 0,
      cache_read_price: 0,
      cache_write_price: 0,
      status: 1,
    })
  })

  it('explains that empty override prices mean zero and confirms an all-zero save', async () => {
    installHandlers()
    let posts = 0
    const bodies: Record<string, unknown>[] = []
    server.use(
      http.post(`${ORIGIN}/api/group-pricing`, async ({ request }) => {
        posts += 1
        bodies.push((await request.json()) as Record<string, unknown>)
        return HttpResponse.json(
          {
            id: 1,
            group: 'vip',
            model: 'gpt-5',
            mode: 'per_token',
            prompt_price: 0,
            completion_price: 0,
            cache_read_price: 0,
            cache_write_price: 0,
            status: 1,
            source_status: 1,
            created_at: '2026-10-01T10:00:00Z',
            updated_at: '2026-10-01T10:00:00Z',
          },
          { status: 201 },
        )
      }),
    )

    const { findByRole, findByText, getByLabelText, getByRole } = await renderSheet()
    const user = userEvent.setup()

    await findByRole('option', { name: 'vip' })
    await user.selectOptions(getByLabelText('Group'), 'vip')
    await findByRole('option', { name: 'gpt-5' })
    await user.selectOptions(getByLabelText('Model'), 'gpt-5')

    // 覆盖价的空输入是 0（免费），提示必须与模型定价的「留空 = 默认」区分。
    expect(
      await findByText('Credits per 1M tokens; integers only. Empty means 0 (free).'),
    ).toBeTruthy()

    await user.click(getByRole('button', { name: 'Save' }))

    const dialog = await findByRole('dialog', {
      name: 'All override prices are zero',
    })
    expect(posts).toBe(0)

    await user.click(within(dialog).getByRole('button', { name: 'Save as free' }))
    await waitFor(() => {
      expect(posts).toBe(1)
    })
    expect(bodies[0]).toMatchObject({
      prompt_price: 0,
      completion_price: 0,
      cache_read_price: 0,
      cache_write_price: 0,
    })
  })

  it('Escape closes only the topmost confirmation and keeps the sheet input', async () => {
    installHandlers()

    const { findByRole, getByLabelText, getByRole, queryByRole } =
      await renderWithProviders(<ControlledSheet />, ['common', 'admin'])
    const user = userEvent.setup()

    await findByRole('option', { name: 'vip' })
    await user.selectOptions(getByLabelText('Group'), 'vip')
    await findByRole('option', { name: 'gpt-5' })
    await user.selectOptions(getByLabelText('Model'), 'gpt-5')
    await user.click(getByRole('button', { name: 'Save' }))

    await findByRole('dialog', { name: 'All override prices are zero' })
    await user.keyboard('{Escape}')

    // 第一次 Escape 只关确认框；底层 Sheet 保留，未提交的输入不丢失。
    await waitFor(() => {
      expect(
        queryByRole('dialog', { name: 'All override prices are zero' }),
      ).toBeNull()
    })
    const sheet = getByRole('dialog', { name: 'New override' })
    expect((within(sheet).getByLabelText('Group') as HTMLSelectElement).value).toBe(
      'vip',
    )

    // 第二次 Escape 才关 Sheet。
    await user.keyboard('{Escape}')
    await waitFor(() => {
      expect(queryByRole('dialog', { name: 'New override' })).toBeNull()
    })
  })

  it('patches an override with its updated_at and status', async () => {
    installHandlers()
    const bodies: Record<string, unknown>[] = []
    server.use(
      http.patch(`${ORIGIN}/api/group-pricing/vip/gpt-5`, async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>)
        return HttpResponse.json(overrideRow({ prompt_price: 450 }))
      }),
    )

    const { getByLabelText, getByRole } = await renderEdit(overrideRow())
    const user = userEvent.setup()

    const prompt = getByLabelText('Input price (cache miss)')
    await user.clear(prompt)
    await user.type(prompt, '450')
    await user.click(getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(bodies).toHaveLength(1)
    })
    expect(bodies[0]).toMatchObject({
      updated_at: '2026-10-01T10:00:00Z',
      prompt_price: 450,
      completion_price: 1200,
      cache_read_price: 40,
      cache_write_price: 120,
      status: 1,
    })
  })

  it('keeps the input and shows the conflict copy when an edit hits 409', async () => {
    installHandlers()
    server.use(
      http.patch(`${ORIGIN}/api/group-pricing/vip/gpt-5`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Conflict' },
          { status: 409 },
        ),
      ),
    )

    const { findByText, getByLabelText, getByRole } = await renderEdit(overrideRow())
    const user = userEvent.setup()

    const prompt = getByLabelText('Input price (cache miss)')
    await user.clear(prompt)
    await user.type(prompt, '450')
    await user.click(getByRole('button', { name: 'Save' }))

    expect(
      await findByText(
        'This pricing was changed by someone else. Reopen the sheet to continue from the latest version.',
      ),
    ).toBeTruthy()
    expect((prompt as HTMLInputElement).value).toBe('450')
  })
})
