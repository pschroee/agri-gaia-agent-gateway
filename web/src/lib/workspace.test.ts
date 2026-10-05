import { describe, expect, it } from "vitest"
import { workspaceSummary } from "@/lib/workspace"

const now = new Date("2026-09-29T18:00:00")

describe("workspaceSummary", () => {
  it("ohne Sicherung", () => {
    expect(workspaceSummary(undefined, now)).toEqual({ text: "Noch nicht gesichert." })
  })

  it("gesichert, heute nur mit Uhrzeit", () => {
    const s = workspaceSummary(
      { size: 1258291, archive_size: 400000, files: 14, saved_at: new Date("2026-09-29T17:05:00").toISOString() },
      now,
    )
    expect(s.text).toBe("Gesichert: 1,2 MiB, 14 Dateien, 17:05")
    expect(s.warning).toBeUndefined()
  })

  it("ausgelassen über der Grenze: Warnung, frühere Sicherung gilt", () => {
    const s = workspaceSummary(
      {
        size: 2048,
        archive_size: 900,
        files: 1,
        saved_at: new Date("2026-09-28T09:00:00").toISOString(),
        skipped_reason: "312,0 MB in /workspace, Grenze 200,0 MB",
        skipped_at: new Date("2026-09-29T17:30:00").toISOString(),
      },
      now,
    )
    expect(s.text).toMatch(/^Gesichert: 2,0 KiB, 1 Datei, 28\.09\.26/)
    expect(s.warning).toBe("Zuletzt nicht gesichert (17:30): 312,0 MB in /workspace, Grenze 200,0 MB. Beim Fortsetzen gilt die Sicherung oben.")
  })

  it("ausgelassen ohne frühere Sicherung", () => {
    const s = workspaceSummary({ size: 0, archive_size: 0, files: 0, skipped_reason: "zu groß" }, now)
    expect(s.text).toBe("Noch nicht gesichert.")
    expect(s.warning).toContain("gehen die Dateien verloren")
  })
})
