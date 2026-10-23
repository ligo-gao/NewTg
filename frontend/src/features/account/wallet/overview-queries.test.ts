import { QueryClient } from '@tanstack/react-query'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import {
  TOPUP_CONFIG_KEY,
  WALLET_SUMMARY_KEY,
} from '../subscription/constants'
import { topupConfigQueryOptions, walletSummaryQueryOptions } from '../topup/queries'

import {
  overviewSubscriptionsQueryOptions,
  recentTransactionsQueryOptions,
  walletUsageSummaryQueryOptions,
  walletUsageTimeseriesQueryOptions,
} from './overview-queries'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function newClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
}

describe('wallet overview query keys', () => {
  it('uses the documented cache-key shapes', () => {
    expect(recentTransactionsQueryOptions().queryKey).toEqual([
      'wallet',
      'transactions',
      'recent',
    ])
    expect(walletUsageSummaryQueryOptions('7d').queryKey).toEqual([
      'wallet',
      'usage-summary',
      '7d',
    ])
    expect(walletUsageTimeseriesQueryOptions('30d').queryKey).toEqual([
      'wallet',
      'usage-timeseries',
      '30d',
    ])
    expect(overviewSubscriptionsQueryOptions().queryKey).toEqual([
      'subscription',
      'mine',
    ])
  })

  it('reuses the single summary/config definitions from subscription constants', () => {
    expect(walletSummaryQueryOptions().queryKey).toBe(WALLET_SUMMARY_KEY)
    expect(topupConfigQueryOptions().queryKey).toBe(TOPUP_CONFIG_KEY)
  })
})

describe('wallet overview request shapes', () => {
  it('asks for the first page of six recent transactions', async () => {
    let search: string | undefined
    server.use(
      http.get(`${ORIGIN}/api/wallet/transactions`, ({ request }) => {
        search = new URL(request.url).search
        return HttpResponse.json({ items: [], total: 0, page: 1, page_size: 6 })
      }),
    )

    await newClient().fetchQuery(recentTransactionsQueryOptions())

    expect(search).toBe('?p=1&page_size=6')
  })

  it('requests the usage summary for the selected range', async () => {
    let search: string | undefined
    server.use(
      http.get(`${ORIGIN}/api/usage-stats/summary`, ({ request }) => {
        search = new URL(request.url).search
        return HttpResponse.json({})
      }),
    )

    await newClient().fetchQuery(walletUsageSummaryQueryOptions('7d'))

    expect(search).toBe('?range=7d')
  })

  it('requests an hourly-bucketed daily timeseries for the selected range', async () => {
    let search: string | undefined
    server.use(
      http.get(`${ORIGIN}/api/usage-stats/timeseries`, ({ request }) => {
        search = new URL(request.url).search
        return HttpResponse.json([])
      }),
    )

    await newClient().fetchQuery(walletUsageTimeseriesQueryOptions('7d'))

    expect(search).toBe('?range=7d&granularity=day')
  })

  it('warm the subscription cache with the same page size as the tab', async () => {
    let search: string | undefined
    server.use(
      http.get(`${ORIGIN}/api/subscriptions`, ({ request }) => {
        search = new URL(request.url).search
        return HttpResponse.json({ items: [], total: 0, page: 1, page_size: 100 })
      }),
    )

    await newClient().fetchQuery(overviewSubscriptionsQueryOptions())

    expect(search).toBe('?page_size=100')
  })
})
