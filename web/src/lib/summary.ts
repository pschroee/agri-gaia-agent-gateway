// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

import type { Approval } from "@/api/types"

/** A summary written by the gateway (issue #72): the first line says what happens, each further line is a detail. */
export type SummaryParts = { head: string; details: string[] }

/** Splits a gateway summary; undefined for an approval or step without one (older entries). */
export function summaryParts(summary: string | undefined): SummaryParts | undefined {
  if (!summary || summary.trim() === "") return undefined
  const [head, ...details] = summary.split("\n")
  return { head, details: details.filter((l) => l.trim() !== "") }
}

/** The technical preview of a platform call without its first line, which repeats `name`. */
export function technicalPreview(a: Approval): string | undefined {
  if (!a.preview || a.preview === a.name) return undefined
  return a.preview.startsWith(a.name) ? a.preview.slice(a.name.length).trim() : a.preview
}
