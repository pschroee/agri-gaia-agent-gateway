// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

import type { Approval, Effect } from "@/api/types"

export const EFFECTS: readonly Effect[] = ["read", "write", "compute", "irreversible"]

export const EFFECT_LABEL: Record<Effect, string> = {
  read: "Read",
  write: "Write",
  compute: "Compute",
  irreversible: "Irreversible",
}

/** Badge colors per class (Tailwind classes). */
export const EFFECT_BADGE: Record<Effect, string> = {
  read: "bg-zinc-100 text-zinc-700 ring-zinc-300",
  write: "bg-emerald-100 text-emerald-800 ring-emerald-300",
  compute: "bg-amber-100 text-amber-800 ring-amber-300",
  irreversible: "bg-red-100 text-red-800 ring-red-400",
}

/** Whether v is one of the four classes. */
export function isEffect(v: unknown): v is Effect {
  return typeof v === "string" && (EFFECTS as readonly string[]).includes(v)
}

/** The class of a platform approval; undefined for other kinds and for missing or unknown values. */
export function approvalEffect(a: Pick<Approval, "kind" | "effect">): Effect | undefined {
  return a.kind === "platform_write" && isEffect(a.effect) ? a.effect : undefined
}
