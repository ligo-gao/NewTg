/**
 * openapi-fetch types every successful `data` as optional; endpoints with a
 * body always return one. This helper turns the optional into a checked value
 * instead of sprinkling non-null assertions across feature code.
 */
export function requireData<T>(result: { data?: T }): T {
  if (result.data === undefined) {
    throw new Error('response body missing from a successful API call')
  }
  return result.data
}
