/**
 * Shared constants for the subscription feature.
 *
 * These used to be copied per module (`TOPUP_CONFIG_KEY` in three places,
 * `newKey()` in two). One definition keeps the React Query cache key and the
 * idempotency-key minting identical everywhere: a drifted key would silently
 * issue duplicate `/api/topup/config` requests, and a drifted key generator
 * would break same-intent replay.
 */

/** Public payment config, shared with the top-up form and playground. */
export const TOPUP_CONFIG_KEY = ['topup', 'config'] as const
/** Wallet balance, shared with the top-up page and playground. */
export const WALLET_SUMMARY_KEY = ['wallet', 'summary'] as const

/**
 * Stable empty list: a fresh `[]` per render would retrigger effects that list
 * it as a dependency. `readonly never[]` is assignable to every provider union
 * used by the callers.
 */
export const EMPTY_PROVIDERS: readonly never[] = []

/** One key per user intent; reused across retries, replaced on dialog reopen. */
export function newIdempotencyKey(): string {
  return globalThis.crypto.randomUUID()
}
