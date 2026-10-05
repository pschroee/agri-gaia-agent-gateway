import { describe, expect, it } from "vitest"
import { formatPeakWindows } from "./tariff"

const deepseek = [
  { days: "mon-fri", from: "01:00", to: "04:00" },
  { days: "mon-fri", from: "06:00", to: "10:00" },
]
const summer = new Date("2026-09-29T12:00:00Z") // CEST, UTC+2
const winter = new Date("2026-12-01T12:00:00Z") // CET, UTC+1

describe("formatPeakWindows", () => {
  it("converts UTC windows in summer time and groups equal days", () => {
    expect(formatPeakWindows(deepseek, "Europe/Berlin", summer)).toBe("Mon–Fri 03:00–06:00 and 08:00–12:00")
  })
  it("converts in winter time with one hour offset", () => {
    expect(formatPeakWindows(deepseek, "Europe/Berlin", winter)).toBe("Mon–Fri 02:00–05:00 and 07:00–11:00")
  })
  it("shifts the days when the window starts after midnight in local time", () => {
    expect(formatPeakWindows([{ days: "mon-fri", from: "23:00", to: "23:30" }], "Europe/Berlin", summer)).toBe(
      "Tue–Sat 01:00–01:30",
    )
  })
  it("shifts the days back for a western time zone", () => {
    expect(formatPeakWindows([{ days: "mon-fri", from: "01:00", to: "04:00" }], "America/New_York", summer)).toBe(
      "Sun–Thu 21:00–00:00",
    )
  })
  it("understands single days, lists and daily", () => {
    expect(formatPeakWindows([{ days: "sat", from: "10:00", to: "11:00" }], "UTC", summer)).toBe("Sat 10:00–11:00")
    expect(formatPeakWindows([{ days: "mon,wed", from: "10:00", to: "11:00" }], "UTC", summer)).toBe(
      "Mon, Wed 10:00–11:00",
    )
    expect(formatPeakWindows([{ days: "daily", from: "10:00", to: "11:00" }], "UTC", summer)).toBe(
      "daily 10:00–11:00",
    )
  })
  it("separates groups with different days by semicolons", () => {
    expect(
      formatPeakWindows(
        [
          { days: "mon-fri", from: "10:00", to: "11:00" },
          { days: "sat-sun", from: "12:00", to: "13:00" },
        ],
        "UTC",
        summer,
      ),
    ).toBe("Mon–Fri 10:00–11:00; Sat–Sun 12:00–13:00")
  })
  it("keeps unintelligible day specs and returns empty text for an empty list", () => {
    expect(formatPeakWindows([{ days: "holidays", from: "10:00", to: "11:00" }], "UTC", summer)).toBe(
      "holidays 10:00–11:00",
    )
    expect(formatPeakWindows([], "UTC", summer)).toBe("")
  })
})
