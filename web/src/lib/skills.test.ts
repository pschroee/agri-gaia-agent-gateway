// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

import { describe, expect, it } from "vitest"
import { skillLabel, skillRead } from "./skills"

const load = (skill: string) => ({ path: `/opt/agw/skills/${skill}/SKILL.md` })

describe("skillRead", () => {
  it("recognises a SKILL.md load", () => {
    const r = skillRead("read", load("internet"))
    expect(r).toEqual({ kind: "load", skill: "internet", path: "/opt/agw/skills/internet/SKILL.md" })
    expect(skillLabel(r!)).toEqual({ label: "Skill loaded", text: "internet" })
  })

  it("accepts names with hyphens and dots", () => {
    expect(skillRead("read", load("web-research"))?.skill).toBe("web-research")
    expect(skillRead("read", load("writing-typst"))?.skill).toBe("writing-typst")
    expect(skillRead("read", load("model-card.v2"))?.skill).toBe("model-card.v2")
  })

  it("treats another file of a skill as a file read, also in a subfolder", () => {
    const r = skillRead("read", { path: "/opt/agw/skills/web-research/references/api.md" })
    expect(r).toMatchObject({ kind: "file", skill: "web-research", file: "references/api.md" })
    expect(skillLabel(r!)).toEqual({ label: "Skill web-research", text: "read references/api.md" })
  })

  it("reads file_path in place of path", () => {
    expect(skillRead("read", { file_path: "/opt/agw/skills/internet/SKILL.md" })?.kind).toBe("load")
  })

  it("ignores arguments that are not an object", () => {
    expect(skillRead("read", "SKILL")).toBeUndefined()
    expect(skillRead("read", undefined)).toBeUndefined()
    expect(skillRead("read", null)).toBeUndefined()
    expect(skillRead("read", ["/opt/agw/skills/x/SKILL.md"])).toBeUndefined()
  })

  it("matches only the read tool", () => {
    const path = "/opt/agw/skills/x/SKILL.md"
    expect(skillRead("bash", { command: `cat ${path}` })).toBeUndefined()
    expect(skillRead("grep", { path: "/opt/agw/skills/x" })).toBeUndefined()
    expect(skillRead("ls", { path: "/opt/agw/skills/x" })).toBeUndefined()
    expect(skillRead("write", { path })).toBeUndefined()
  })

  it("rejects other prefixes, names and unsafe paths", () => {
    const bad = [
      "/opt/agw/skillsX/x/SKILL.md",
      "/opt/agw/other/x/SKILL.md",
      "/workspace/SKILL.md",
      "/opt/agw/skills/SKILL.md",
      "/opt/agw/skills/x/../SKILL.md",
      "/opt/agw/skills/x/./SKILL.md",
      "/opt/agw/skills/x//SKILL.md",
      "/opt/agw/skills/x/",
    ]
    for (const path of bad) expect(skillRead("read", { path })).toBeUndefined()
  })

  it("does not count skill.md in lower case as the load", () => {
    expect(skillRead("read", { path: "/opt/agw/skills/x/skill.md" })?.kind).toBe("file")
  })
})
