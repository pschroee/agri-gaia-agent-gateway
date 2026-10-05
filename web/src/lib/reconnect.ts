/** Delay before the n-th reconnection attempt (0 = first): 2 s, doubled, at most 30 s. */
export function reconnectDelay(attempt: number): number {
  const n = Math.max(0, attempt)
  return Math.min(30_000, 2_000 * 2 ** n)
}
