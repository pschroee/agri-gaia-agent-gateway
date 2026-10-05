import type { Delegation, DelegationRule } from "@/api/types"

/** Template for the delegated rights of a new chat (internal/delegation). */
export type DelegationTemplate = { id: string; label: string; description: string; rules?: DelegationRule[] }

/** Resources covered by "Read only" (all except api). */
export const READ_RESOURCES = ["dataset", "model", "training", "task", "train_template", "edge_device", "container_image"]

const readAll: DelegationRule[] = READ_RESOURCES.map((resource) => ({ action: "read", resource, ids: ["*"] }))

export const DELEGATION_TEMPLATES: DelegationTemplate[] = [
  {
    id: "none",
    label: "No delegation",
    description: "Reading needs no confirmation, writing needs an approval.",
  },
  {
    id: "read",
    label: "Read only",
    description: "The agent may read datasets, models, trainings, tasks, templates, edge devices and images, nothing else.",
    rules: readAll,
  },
  {
    id: "train-own",
    label: "Training on own dataset",
    description:
      "Read everything as with \"Read only\"; create datasets and change or delete only those it created; create and start trainings. " +
      "Training containers are created asynchronously and cannot be attributed to a chat; start and stop therefore apply to all containers.",
    rules: [
      ...readAll,
      { action: "create", resource: "dataset" },
      { action: "update", resource: "dataset", ids: ["own"] },
      { action: "delete", resource: "dataset", ids: ["own"] },
      { action: "create", resource: "training" },
      { action: "run", resource: "training", ids: ["*"] },
    ],
  },
]

export const DEFAULT_DELEGATION_HOURS = 8

/** Delegation from a template, valid for `hours` hours from `now`; undefined for "No delegation". */
export function delegationFrom(t: DelegationTemplate | undefined, hours: number, now: Date = new Date()): Delegation | undefined {
  if (!t?.rules) return undefined
  return {
    rules: t.rules.map((r) => ({ ...r, ids: r.ids ? [...r.ids] : undefined })),
    expires_at: new Date(now.getTime() + hours * 3600_000).toISOString(),
    enforce: true,
  }
}

// Matches violations in socket log results ("violation blocked: …" or "… · violation, logged only: …").
const VIOLATION = /violation/i

/** Whether a socket log result records a violation. */
export function isViolation(result: string | undefined): boolean {
  return VIOLATION.test(result ?? "")
}

/** Violations in the socket log. */
export function countViolations(calls: { result?: string }[]): number {
  return calls.filter((c) => isViolation(c.result)).length
}
