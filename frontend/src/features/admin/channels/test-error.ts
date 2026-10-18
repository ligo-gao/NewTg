/**
 * Maps the backend test `error_class` enum (relay-02 ClassifyError) to the
 * i18n key used by the list toast and the batch-test dialog.
 */
const TEST_ERROR_KEYS: Record<string, string> = {
  auth: 'common.channels.testError.auth',
  timeout: 'common.channels.testError.timeout',
  server: 'common.channels.testError.server',
  network: 'common.channels.testError.network',
  param: 'common.channels.testError.param',
}

export function testErrorKey(errorClass: string | undefined): string {
  if (errorClass === undefined) return 'common.channels.testError.unknown'
  return TEST_ERROR_KEYS[errorClass] ?? 'common.channels.testError.unknown'
}
