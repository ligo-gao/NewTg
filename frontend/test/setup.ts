import { webcrypto } from 'node:crypto'

import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

/**
 * jsdom ships `Crypto.getRandomValues` but not `SubtleCrypto`. The login
 * encryption path (F-01-25) needs RSA-OAEP and AES-256-GCM, so tests install
 * Node's WebCrypto implementation. Tests never embed real key material: every
 * key pair is generated ephemerally inside the test that needs one.
 */
if (globalThis.crypto?.subtle === undefined) {
  Object.defineProperty(globalThis, 'crypto', {
    configurable: true,
    value: webcrypto,
  })
}

// Vitest runs with `globals: false`, so Testing Library cannot register its
// own auto-cleanup; unmount between tests explicitly to keep DOM state isolated.
afterEach(() => {
  cleanup()
})
