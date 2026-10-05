import { describe, expect, it } from "vitest"
import { workspaceSummary } from "@/lib/workspace"

const now = new Date("2026-09-29T18:00:00")

describe("workspaceSummary", () => {
  it("without backup", () => {
    expect(workspaceSummary(undefined, now)).toEqual({ text: "Not backed up yet." })
  })

  it("backed up, today only with the time", () => {
    const s = workspaceSummary(
      { size: 1258291, archive_size: 400000, files: 14, saved_at: new Date("2026-09-29T17:05:00").toISOString() },
      now,
    )
    expect(s.text).toBe("Backed up: 1.2 MiB, 14 files, 17:05")
    expect(s.warning).toBeUndefined()
  })

  it("skipped above the limit: warning, earlier backup applies", () => {
    const s = workspaceSummary(
      {
        size: 2048,
        archive_size: 900,
        files: 1,
        saved_at: new Date("2026-09-28T09:00:00").toISOString(),
        skipped_reason: "312.0 MB in /workspace, limit 200.0 MB",
        skipped_at: new Date("2026-09-29T17:30:00").toISOString(),
      },
      now,
    )
    expect(s.text).toMatch(/^Backed up: 2\.0 KiB, 1 file, 9\/28\/26/)
    expect(s.warning).toBe("Last backup skipped (17:30): 312.0 MB in /workspace, limit 200.0 MB. On resume, the backup above applies.")
  })

  it("skipped without an earlier backup", () => {
    const s = workspaceSummary({ size: 0, archive_size: 0, files: 0, skipped_reason: "too large" }, now)
    expect(s.text).toBe("Not backed up yet.")
    expect(s.warning).toContain("the files are lost")
  })
})
