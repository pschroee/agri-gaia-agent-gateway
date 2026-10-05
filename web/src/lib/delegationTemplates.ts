import type { Delegation, DelegationRule } from "@/api/types"

/** Vorlage für die übertragenen Rechte eines neuen Chats (internal/delegation). */
export type DelegationTemplate = { id: string; label: string; description: string; rules?: DelegationRule[] }

/** Ressourcen, die „Nur lesen“ umfasst (alle außer api). */
export const READ_RESOURCES = ["dataset", "model", "training", "task", "train_template", "edge_device", "container_image"]

const readAll: DelegationRule[] = READ_RESOURCES.map((resource) => ({ action: "read", resource, ids: ["*"] }))

export const DELEGATION_TEMPLATES: DelegationTemplate[] = [
  {
    id: "none",
    label: "Ohne Delegation",
    description: "Lesen geht ohne Rückfrage, Schreiben braucht eine Bestätigung.",
  },
  {
    id: "read",
    label: "Nur lesen",
    description: "Der Agent darf Datensätze, Modelle, Trainings, Aufgaben, Vorlagen, Edge-Geräte und Abbilder lesen, sonst nichts.",
    rules: readAll,
  },
  {
    id: "train-own",
    label: "Training auf eigenem Datensatz",
    description:
      "Alles lesen wie bei „Nur lesen“; Datensätze anlegen und nur selbst angelegte ändern oder löschen; Training anlegen und starten. " +
      "Trainingscontainer entstehen asynchron und lassen sich keinem Chat zuordnen; Start und Stopp gelten deshalb für alle Container.",
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

/** Delegation aus einer Vorlage, gültig `hours` Stunden ab `now`; undefined bei „Ohne Delegation“. */
export function delegationFrom(t: DelegationTemplate | undefined, hours: number, now: Date = new Date()): Delegation | undefined {
  if (!t?.rules) return undefined
  return {
    rules: t.rules.map((r) => ({ ...r, ids: r.ids ? [...r.ids] : undefined })),
    expires_at: new Date(now.getTime() + hours * 3600_000).toISOString(),
    enforce: true,
  }
}

/** Übergriffe im Socket-Protokoll (Ergebnis „übergriff abgewiesen: …“ bzw. „… übergriff, nur protokolliert“). */
export function countViolations(calls: { result?: string }[]): number {
  return calls.filter((c) => /übergriff/i.test(c.result ?? "")).length
}
