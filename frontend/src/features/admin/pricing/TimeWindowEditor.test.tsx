import { useState } from 'react'
import userEvent from '@testing-library/user-event'
import { fireEvent, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { TimeWindowEditor } from './TimeWindowEditor'
import { createDefaultTimeConfig, type TimeConfigDraft } from './pricing-utils'

function Harness({ onChange }: { onChange?: (next: TimeConfigDraft) => void }) {
  const [value, setValue] = useState<TimeConfigDraft>(createDefaultTimeConfig())
  return (
    <TimeWindowEditor
      value={value}
      onChange={(next) => {
        setValue(next)
        onChange?.(next)
      }}
    />
  )
}

describe('TimeWindowEditor', () => {
  it('starts disabled and explains the base-rate behaviour', async () => {
    const { getByLabelText, getByText, queryByRole } = await renderWithProviders(
      <Harness />,
      ['common', 'admin'],
    )

    expect((getByLabelText('Enable time pricing') as HTMLInputElement).checked).toBe(false)
    expect(getByText('When disabled, the whole day uses the base rates.')).toBeTruthy()
    expect(queryByRole('button', { name: 'Add window' })).toBeNull()
  })

  it('adds and removes windows and toggles weekdays', async () => {
    const onChange = vi.fn()
    const { getByLabelText, getByRole, queryByRole, findByRole } =
      await renderWithProviders(<Harness onChange={onChange} />, ['common', 'admin'])
    const user = userEvent.setup()

    await user.click(getByLabelText('Enable time pricing'))
    expect(getByRole('button', { name: 'Add window' })).toBeTruthy()

    await user.click(getByRole('button', { name: 'Add window' }))
    const window = await findByRole('group', { name: 'Window 1' })
    const monday = within(window).getByRole('button', { name: 'Monday' })
    const sunday = within(window).getByRole('button', { name: 'Sunday' })
    expect(monday.getAttribute('aria-pressed')).toBe('true')
    expect(sunday.getAttribute('aria-pressed')).toBe('false')

    await user.click(sunday)
    expect(sunday.getAttribute('aria-pressed')).toBe('true')
    await user.click(sunday)
    expect(sunday.getAttribute('aria-pressed')).toBe('false')

    const last = onChange.mock.calls.at(-1)?.[0] as TimeConfigDraft
    expect(last.windows[0]?.days).toEqual([1, 2, 3, 4, 5])

    await user.click(within(window).getByRole('button', { name: 'Remove window 1' }))
    expect(queryByRole('group', { name: 'Window 1' })).toBeNull()
  })

  it('explains overnight windows from the start day', async () => {
    const { getByLabelText, getByRole, findByText } = await renderWithProviders(
      <Harness />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    await user.click(getByLabelText('Enable time pricing'))
    await user.click(getByRole('button', { name: 'Add window' }))

    const start = getByLabelText('Start 1')
    const end = getByLabelText('End 1')
    fireEvent.change(start, { target: { value: '22:00' } })
    fireEvent.change(end, { target: { value: '08:00' } })

    expect(
      await findByText(/Overnight window: Monday 22:00 to Tuesday 08:00/),
    ).toBeTruthy()
  })

  it('fills a weekday peak/off-peak schedule in one click', async () => {
    const onChange = vi.fn()
    const { getByRole, getByText } = await renderWithProviders(
      <Harness onChange={onChange} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    expect(getByText(/Weekdays 09:00–12:00 and 14:00–18:00/)).toBeTruthy()
    await user.click(getByRole('button', { name: 'Weekday peak/off-peak' }))

    const next = onChange.mock.calls.at(-1)?.[0] as TimeConfigDraft
    expect(next).toMatchObject({
      enabled: true,
      timezone: 'Asia/Shanghai',
      holidayPolicy: 'cn_offpeak',
      defaultMultiplier: '50',
    })
    expect(
      next.windows.map((window) => [window.start, window.end, window.multiplier]),
    ).toEqual([
      ['09:00', '12:00', '100'],
      ['14:00', '18:00', '100'],
    ])
    expect(next.windows.every((window) => window.days.length === 5)).toBe(true)
  })

  it('replaces existing windows instead of merging them', async () => {
    const onChange = vi.fn()
    const { getByLabelText, getByRole } = await renderWithProviders(
      <Harness onChange={onChange} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    await user.click(getByLabelText('Enable time pricing'))
    await user.click(getByRole('button', { name: 'Add window' }))
    await user.click(getByRole('button', { name: 'Add window' }))
    await user.click(getByRole('button', { name: 'Weekday peak/off-peak' }))

    const next = onChange.mock.calls.at(-1)?.[0] as TimeConfigDraft
    expect(next.windows).toHaveLength(2)
  })
})
