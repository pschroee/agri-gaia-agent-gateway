import { ApiError, type ContextTooLarge } from "@/api/client"

/** Liefert die Details, wenn ein Modellwechsel am zu vollen Kontext scheiterte. */
export function contextTooLarge(e: unknown): ContextTooLarge | undefined {
  return e instanceof ApiError && e.code === "context_too_large" ? (e.details as ContextTooLarge) : undefined
}
