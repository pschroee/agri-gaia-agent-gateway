// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

/** Reads of the agent's skills in the tool cards (issue #73); the same rule as the frontend's `skillStep.ts`. */

export type SkillRead = { kind: "load" | "file"; skill: string; file?: string; path: string }

/** `/opt/agw/skills/<name>/<rest>`; the name starts with a letter or digit, no normalisation of the path. */
const SKILL_PATH = /^\/opt\/agw\/skills\/([A-Za-z0-9][A-Za-z0-9._-]*)\/(.+)$/

/**
 * Whether a tool call is a `read` of a skill file. Only the tool `read` counts, and only its arguments: `path`, or
 * `file_path` in its place. `bash`, `grep`, `ls` and the like stay ordinary tool calls.
 */
export function skillRead(name: string, args: unknown): SkillRead | undefined {
  if (name !== "read") return undefined
  if (!args || typeof args !== "object" || Array.isArray(args)) return undefined
  const a = args as Record<string, unknown>
  const path = typeof a.path === "string" ? a.path : a.file_path
  if (typeof path !== "string") return undefined
  const m = SKILL_PATH.exec(path)
  if (!m) return undefined
  const rest = m[2]
  if (rest.split("/").some((segment) => segment === "" || segment === "." || segment === "..")) return undefined
  if (rest === "SKILL.md") return { kind: "load", skill: m[1], path }
  return { kind: "file", skill: m[1], file: rest, path }
}

/** Label and text of a card: "Skill loaded" / "internet", or "Skill web-research" / "read references/api.md". */
export function skillLabel(r: SkillRead): { label: string; text: string } {
  if (r.kind === "load") return { label: "Skill loaded", text: r.skill }
  return { label: `Skill ${r.skill}`, text: `read ${r.file}` }
}
