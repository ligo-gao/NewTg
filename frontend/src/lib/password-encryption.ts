/**
 * Login password client encryption (F-01-25 / modules-01 §3.14).
 *
 * Envelope format frozen with the backend (`auth.EncryptPasswordV2`):
 *
 *   v2.<b64(wrappedKey)>.<b64(nonce)>.<b64(ciphertext)>
 *
 * The one-time AES-256-GCM data key is wrapped with RSA-OAEP(SHA-256) using
 * the label `password-v2`; the ciphertext is bound to the key slot with
 * AAD `password-v2:<kid>`. Standard base64 **with padding** is used because
 * that is what the server decoder accepts.
 *
 * Anti-downgrade (I-9): when the endpoint reports encryption enabled, a failure
 * to obtain or parse the key throws — it never falls back to a plaintext
 * `password` field. Only `enabled === false` selects the plaintext path; a
 * response with enabled missing or true but no key material is an error.
 */

const KEY_ENDPOINT = '/api/auth/login/encryption-key'
const KEY_CACHE_TTL_MS = 5 * 60 * 1000
const ENVELOPE_VERSION = 'v2'
const OAEP_LABEL = 'password-v2'
const AAD_PREFIX = 'password-v2:'
const NONCE_BYTES = 12
const AES_KEY_BITS = 256

export type LoginEncryptionKey =
  | { enabled: false }
  | { enabled: true; kid: string; publicKey: string }

/**
 * The key endpoint cannot be used (network, 5xx, or malformed body). Callers
 * must show a retryable error; there is no plaintext fallback in encrypted
 * mode (I-9).
 */
export class LoginEncryptionUnavailableError extends Error {
  constructor(reason: string) {
    super(`login encryption unavailable: ${reason}`)
    this.name = 'LoginEncryptionUnavailableError'
  }
}

export interface PlainLoginCredentials {
  identifier: string
  password: string
}

export interface EncryptedLoginCredentials {
  identifier: string
  password_encrypted: string
  encryption_key_id: string
}

export type LoginCredentials = PlainLoginCredentials | EncryptedLoginCredentials

interface KeyOptions {
  /** Injected by tests; production uses global fetch. */
  fetcher?: typeof fetch
  /** Injected clock so the 5-minute cache is testable. */
  now?: () => number
}

let cachedKey: { expiresAt: number; value: LoginEncryptionKey } | undefined

export function clearLoginEncryptionKeyCache(): void {
  cachedKey = undefined
}

function currentOrigin(): string {
  return typeof location !== 'undefined' ? location.origin : 'http://localhost'
}

function base64(bytes: Uint8Array): string {
  return btoa(String.fromCharCode(...bytes))
}

function pemBodyToBytes(pem: string): Uint8Array<ArrayBuffer> {
  const body = pem
    .replace(/-----BEGIN PUBLIC KEY-----/, '')
    .replace(/-----END PUBLIC KEY-----/, '')
    .replace(/\s+/g, '')
  const binary = atob(body)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i)
  return bytes
}

/**
 * Fetches and caches the login public key for five minutes. The server's
 * `Cache-Control: no-store` disallows intermediary caching, not this short
 * client-side cache the rotation design (I-7) explicitly budgeted for.
 */
export async function fetchLoginEncryptionKey(
  options: KeyOptions = {},
): Promise<LoginEncryptionKey> {
  const now = options.now?.() ?? Date.now()
  if (cachedKey !== undefined && cachedKey.expiresAt > now) {
    return cachedKey.value
  }
  cachedKey = undefined

  const fetcher = options.fetcher ?? fetch
  let response: Response
  try {
    response = await fetcher(new URL(KEY_ENDPOINT, currentOrigin()).toString(), {
      method: 'GET',
      credentials: 'same-origin',
      headers: { Accept: 'application/json' },
    })
  } catch {
    throw new LoginEncryptionUnavailableError('network')
  }
  if (!response.ok) {
    // No plaintext fallback: a broken key endpoint must fail closed.
    throw new LoginEncryptionUnavailableError(`status ${response.status}`)
  }

  const body: unknown = await response.json()
  const record =
    typeof body === 'object' && body !== null
      ? (body as Record<string, unknown>)
      : undefined

  let value: LoginEncryptionKey
  if (record?.enabled === false) {
    value = { enabled: false }
  } else {
    const kid = record?.kid
    const publicKey = record?.public_key
    if (typeof kid !== 'string' || kid === '' || typeof publicKey !== 'string') {
      throw new LoginEncryptionUnavailableError('missing key material')
    }
    value = { enabled: true, kid, publicKey }
  }

  cachedKey = { expiresAt: now + KEY_CACHE_TTL_MS, value }
  return value
}

/**
 * Encrypts a password into the v2 envelope. Long/multi-byte passwords are the
 * reason v2 exists: bare RSA-OAEP cannot fit more than one block.
 */
export async function encryptPasswordV2(
  password: string,
  publicKeyPEM: string,
  kid: string,
): Promise<string> {
  if (password === '') throw new Error('password must not be empty')
  if (kid === '') throw new Error('encryption key id is required')

  const encoder = new TextEncoder()
  const publicKey = await crypto.subtle.importKey(
    'spki',
    pemBodyToBytes(publicKeyPEM),
    { name: 'RSA-OAEP', hash: 'SHA-256' },
    false,
    ['encrypt'],
  )

  // One data key per login attempt: no nonce reuse is possible under a
  // (key, nonce) pair because the AES key itself is fresh every time.
  const dataKey = await crypto.subtle.generateKey(
    { name: 'AES-GCM', length: AES_KEY_BITS },
    true,
    ['encrypt'],
  )
  const rawDataKey = await crypto.subtle.exportKey('raw', dataKey)

  const wrappedKey = await crypto.subtle.encrypt(
    { name: 'RSA-OAEP', label: encoder.encode(OAEP_LABEL) },
    publicKey,
    rawDataKey,
  )

  const nonce = crypto.getRandomValues(new Uint8Array(NONCE_BYTES))
  const ciphertext = await crypto.subtle.encrypt(
    {
      name: 'AES-GCM',
      iv: nonce,
      additionalData: encoder.encode(`${AAD_PREFIX}${kid}`),
    },
    dataKey,
    encoder.encode(password),
  )

  return [
    ENVELOPE_VERSION,
    base64(new Uint8Array(wrappedKey)),
    base64(nonce),
    base64(new Uint8Array(ciphertext)),
  ].join('.')
}

/**
 * Builds the `POST /api/auth/login` credential fields honoring the server
 * switch. Throws when the key cannot be obtained in encrypted mode.
 */
export async function buildLoginCredentials(
  identifier: string,
  password: string,
  options: KeyOptions = {},
): Promise<LoginCredentials> {
  const key = await fetchLoginEncryptionKey(options)
  if (!key.enabled) {
    return { identifier, password }
  }
  return {
    identifier,
    password_encrypted: await encryptPasswordV2(password, key.publicKey, key.kid),
    encryption_key_id: key.kid,
  }
}
