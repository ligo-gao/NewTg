import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { CheckinCard } from './checkin-card'

const ORIGIN = 'http://localhost:3000'

function statusBody(overrides: Record<string, unknown> = {}) {
  return {
    system_initialized: true,
    auth: {
      signup_enabled: true,
      password_login_enabled: true,
      email_verification_required: false,
      turnstile: { enabled: false, site_key: '' },
    },
    password_login_encryption_enabled: false,
    checkin_enabled: true,
    languages: ['en'],
    default_language: 'en',
    site: {},
    nav: {},
    billing: {},
    pay: {},
    usd_exchange_rate: 7.1,
    credit_display_symbol: 'C',
    usable_groups: [],
    ...overrides,
  }
}

function calendar(overrides: Record<string, unknown> = {}) {
  return {
    month: '2026-10',
    today: '2026-10-22',
    days: [],
    streak: 0,
    month_total: 0,
    total_earned: 0,
    checked_in_today: false,
    ...overrides,
  }
}

const server = setupServer()
beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function serveStatus(body: Record<string, unknown>) {
  server.use(http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(body)))
}

function serveCalendar(body: Record<string, unknown>) {
  server.use(http.get(`${ORIGIN}/api/checkin/calendar`, () => HttpResponse.json(body)))
}

describe('CheckinCard (F-01-30)', () => {
  it('renders nothing when the public status flag is off', async () => {
    serveStatus(statusBody({ checkin_enabled: false }))
    let calendarCalls = 0
    server.use(
      http.get(`${ORIGIN}/api/checkin/calendar`, () => {
        calendarCalls += 1
        return HttpResponse.json(calendar())
      }),
    )
    await renderWithProviders(<CheckinCard />, ['common'])
    await waitFor(() => expect(screen.queryByTestId('checkin-card')).toBeNull())
    expect(calendarCalls).toBe(0)
  })

  it('shows the empty calendar and an enabled check-in button', async () => {
    serveStatus(statusBody())
    serveCalendar(calendar())
    await renderWithProviders(<CheckinCard />, ['common'])

    expect(await screen.findByTestId('checkin-card')).toBeTruthy()
    const button = (await screen.findByTestId('checkin-button')) as HTMLButtonElement
    expect(button.disabled).toBe(false)
    expect(button.textContent).toBe('Check in')
    expect(await screen.findByTestId('checkin-empty')).toBeTruthy()
    expect(screen.getByTestId('checkin-streak').textContent).toBe('0-day streak')
  })

  it('renders a checked-in day, streak, month total and today award', async () => {
    serveStatus(statusBody())
    serveCalendar(
      calendar({
        days: [
          { date: '2026-10-21', awarded: 5 },
          { date: '2026-10-22', awarded: 7 },
        ],
        streak: 2,
        month_total: 12,
        total_earned: 99,
        checked_in_today: true,
      }),
    )
    await renderWithProviders(<CheckinCard />, ['common'])

    // Wait for the calendar payload (the button starts disabled while pending).
    await screen.findByTestId('checkin-streak')
    const button = screen.getByTestId('checkin-button') as HTMLButtonElement
    expect(button.disabled).toBe(true)
    expect(button.textContent).toBe('Checked in today')
    expect(screen.getByTestId('checkin-streak').textContent).toBe('2-day streak')
    expect(screen.getByTestId('checkin-month-total').textContent).toBe('12')
    expect(screen.getByTestId('checkin-total-earned').textContent).toBe('99')
    expect(screen.getByTestId('checkin-today-award').textContent).toBe(
      "Today's reward: 7 credits",
    )
    // 月历 7 列：已签到的 10-21 / 10-22 可被单独定位。
    expect(await screen.findByTestId('checkin-day-2026-10-21')).toBeTruthy()
    expect(screen.getByTestId('checkin-day-2026-10-22')).toBeTruthy()
  })

  it('submits without any user id and refreshes the calendar after success', async () => {
    let checked = false
    let postedBody: unknown
    serveStatus(statusBody())
    server.use(
      http.get(`${ORIGIN}/api/checkin/calendar`, () =>
        HttpResponse.json(
          calendar(
            checked
              ? {
                  days: [{ date: '2026-10-22', awarded: 37 }],
                  streak: 1,
                  month_total: 37,
                  total_earned: 37,
                  checked_in_today: true,
                }
              : {},
          ),
        ),
      ),
      http.post(`${ORIGIN}/api/checkin`, async ({ request }) => {
        postedBody = await request.json()
        checked = true
        return HttpResponse.json({
          date: '2026-10-22',
          awarded: 37,
          streak: 1,
          month_total: 37,
          total_earned: 37,
          balance_after: 1037,
          checked_in_today: true,
        })
      }),
    )
    await renderWithProviders(<CheckinCard />, ['common'])

    const user = userEvent.setup()
    await user.click(await screen.findByTestId('checkin-button'))

    expect(await screen.findByTestId('checkin-feedback')).toBeTruthy()
    expect(screen.getByTestId('checkin-feedback').textContent).toBe(
      'Checked in. You earned 37 credits',
    )
    expect(postedBody).toEqual({})
    await waitFor(() => {
      const button = screen.getByTestId('checkin-button') as HTMLButtonElement
      expect(button.disabled).toBe(true)
      expect(button.textContent).toBe('Checked in today')
    })
  })

  it('treats a same-day 409 as an already-checked replay', async () => {
    serveStatus(statusBody())
    serveCalendar(calendar())
    server.use(
      http.post(`${ORIGIN}/api/checkin`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/already_checked_in',
            title: 'Already checked in',
            status: 409,
          },
          { status: 409 },
        ),
      ),
    )
    await renderWithProviders(<CheckinCard />, ['common'])
    const user = userEvent.setup()
    await user.click(await screen.findByTestId('checkin-button'))

    const feedback = await screen.findByTestId('checkin-feedback')
    expect(feedback.textContent).toBe('You have already checked in today')
    expect(screen.queryByTestId('checkin-mutation-error')).toBeNull()
  })

  it('shows an inline error with retry when the calendar cannot load', async () => {
    serveStatus(statusBody())
    let calls = 0
    server.use(
      http.get(`${ORIGIN}/api/checkin/calendar`, () => {
        calls += 1
        if (calls === 1) {
          return HttpResponse.json({ title: 'boom', status: 500 }, { status: 500 })
        }
        return HttpResponse.json(calendar())
      }),
    )
    await renderWithProviders(<CheckinCard />, ['common'])

    expect(await screen.findByTestId('checkin-error')).toBeTruthy()
    const user = userEvent.setup()
    await user.click(screen.getByTestId('checkin-retry'))
    expect(await screen.findByTestId('checkin-empty')).toBeTruthy()
  })

  it('surfaces offline on a transport failure', async () => {
    serveStatus(statusBody())
    serveCalendar(calendar())
    server.use(http.post(`${ORIGIN}/api/checkin`, () => HttpResponse.error()))
    await renderWithProviders(<CheckinCard />, ['common'])
    const user = userEvent.setup()
    await user.click(await screen.findByTestId('checkin-button'))

    expect(await screen.findByTestId('checkin-offline')).toBeTruthy()
  })
})

// seven-language dictionary parity for the keys this card renders.
describe('checkin locale parity (7 languages)', () => {
  const locales = ['en', 'zh-CN', 'zh-TW', 'fr', 'ru', 'ja', 'vi'] as const
  const required = [
    'title',
    'description',
    'button',
    'checked_button',
    'submitting',
    'success',
    'already',
    'streak',
    'streak_label',
    'month_total_label',
    'total_earned_label',
    'empty',
    'error',
    'offline',
    'action_error',
    'calendar_aria',
    'day_awarded',
    'today_award',
  ] as const

  it('has every key and matching placeholders in all seven dictionaries', async () => {
    const loaders = {
      en: () => import('@/locales/en/common.json'),
      'zh-CN': () => import('@/locales/zh-CN/common.json'),
      'zh-TW': () => import('@/locales/zh-TW/common.json'),
      fr: () => import('@/locales/fr/common.json'),
      ru: () => import('@/locales/ru/common.json'),
      ja: () => import('@/locales/ja/common.json'),
      vi: () => import('@/locales/vi/common.json'),
    } as const

    const placeholders = (text: string) =>
      [...text.matchAll(/\{([a-zA-Z0-9_]+)\}/g)].map((match) => match[1]).sort()

    const dictionaries = Object.fromEntries(
      await Promise.all(
        locales.map(async (locale) => [locale, (await loaders[locale]()).default]),
      ),
    ) as Record<(typeof locales)[number], { dashboard: { checkin: Record<string, string> } }>

    const baseline = dictionaries.en.dashboard.checkin
    for (const locale of locales) {
      const block = dictionaries[locale].dashboard.checkin
      for (const key of required) {
        const translated = block[key] ?? ''
        expect(translated, `${locale}: ${key}`).toBeTruthy()
        expect(placeholders(translated), `${locale}: ${key}`).toEqual(
          placeholders(baseline[key] ?? ''),
        )
      }
    }
  })
})
