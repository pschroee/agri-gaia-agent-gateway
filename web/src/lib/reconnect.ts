/** Wartezeit vor dem n-ten erneuten Verbindungsversuch (0 = erster): 2 s, verdoppelt, höchstens 30 s. */
export function reconnectDelay(attempt: number): number {
  const n = Math.max(0, attempt)
  return Math.min(30_000, 2_000 * 2 ** n)
}
