import { ApiError, type ContextTooLarge } from "@/api/client"

/** Returns the details if a model switch failed because the context is too full. */
export function contextTooLarge(e: unknown): ContextTooLarge | undefined {
  return e instanceof ApiError && e.code === "context_too_large" ? (e.details as ContextTooLarge) : undefined
}
