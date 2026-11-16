// Verdict of a run's audit hash chain, read off the message the server sends.
//
// Pure so it can be unit-tested without mounting the view (this repo has no
// @vue/test-utils), and separate from the view because the mapping is a
// CONTRACT, not a cosmetic choice.
//
// Why the message and not just the boolean:
//
// The production chain is sealed when a run reaches a terminal state, so a
// running change's tail rows carry no hash yet. The verifier fails closed on
// that — an empty hash must never read as "fine" — so `valid` is false for a
// chain that is perfectly intact but not sealed yet. A console that only knew
// the boolean would paint every running change red and send an operator
// hunting for an attacker who does not exist.
//
// So the server guarantees four message PREFIXES (docs/levee-api.md §13.2c)
// and this module maps them to what the reader sees:
//
//   "hash chain valid: …"          → 哈希链完整      (success)
//   "hash chain not sealed yet: …" → 哈希链未封口    (warning)
//   "hash chain broken: …"         → 哈希链未通过    (danger)
//   "no traces to verify"          → 暂无可校验的链  (info)
//
// The prefixes are a contract: the server documents them, the view depends on
// them, and the fallback below is for an UNRECOGNISED message only — it must
// not become the normal path, because then the server could change a message
// and the console would quietly go back to guessing.

/** The tag type the verdict is rendered with (Element Plus tag types). */
export type ChainVerdictType = 'success' | 'danger' | 'info' | 'warning'

export interface ChainVerdict {
  /** What the operator reads. */
  label: string
  /** How it is coloured. */
  type: ChainVerdictType
}

// The prefixes, named once so the view and the tests cannot disagree about
// the spelling.
export const CHAIN_MESSAGE_PREFIXES = {
  valid: 'hash chain valid:',
  notSealed: 'hash chain not sealed yet:',
  broken: 'hash chain broken:',
  noTraces: 'no traces to verify',
} as const

/**
 * chainVerdict maps the server's verification message to a reader-facing
 * verdict.
 *
 * `valid` is the fallback for a message that carries no known prefix: it is
 * all the information there is in that case, and the conservative reading of
 * it (a chain that does not verify is a chain that did not verify) is the one
 * that cannot mislead.
 */
export function chainVerdict(message: string, valid: boolean): ChainVerdict {
  const msg = message || ''
  if (msg.startsWith(CHAIN_MESSAGE_PREFIXES.valid)) {
    return { label: '哈希链完整', type: 'success' }
  }
  if (msg.startsWith(CHAIN_MESSAGE_PREFIXES.notSealed)) {
    return { label: '哈希链未封口', type: 'warning' }
  }
  if (msg.startsWith(CHAIN_MESSAGE_PREFIXES.broken)) {
    return { label: '哈希链未通过', type: 'danger' }
  }
  if (msg.startsWith(CHAIN_MESSAGE_PREFIXES.noTraces)) {
    return { label: '暂无可校验的链', type: 'info' }
  }
  return valid
    ? { label: '哈希链完整', type: 'success' }
    : { label: '哈希链未通过', type: 'danger' }
}

/**
 * chainModeLabel names the digest the chain was built with.
 *
 * The label alone ("哈希链完整") does not carry the guarantee: on an unkeyed
 * deployment anyone who can write to the store can recompute the whole chain,
 * so the reader is told which of the two they are looking at.
 */
export function chainModeLabel(mode: { mode: string; algorithm: string; keyed: boolean }): {
  label: string
  type: ChainVerdictType
} {
  return mode.keyed
    ? { label: `口令摘要 · ${mode.algorithm}`, type: 'success' }
    : { label: `无口令摘要 · ${mode.algorithm}`, type: 'warning' }
}
