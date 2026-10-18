/**
 * Public Button entry point. The Base UI primitive lives in `./base/button`
 * (copied source, web-00 §1.1 item 5); this module is the stable import path
 * for feature code and re-exports the four semantic variants:
 * `primary | secondary | ghost | danger`.
 *
 * The copy for a danger primary button ("Delete permanently", "Revoke") is the
 * caller's responsibility — the primitive only owns shape and state styling.
 */
export {
  Button,
  type ButtonProps,
  type ButtonVariant,
} from './base/button'
