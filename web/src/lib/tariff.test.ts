import { describe, expect, it } from "vitest"
import { formatPeakWindows } from "./tariff"

const deepseek = [
  { days: "mon-fri", from: "01:00", to: "04:00" },
  { days: "mon-fri", from: "06:00", to: "10:00" },
]
const summer = new Date("2026-09-29T12:00:00Z") // MESZ, UTC+2
const winter = new Date("2026-12-01T12:00:00Z") // MEZ, UTC+1

describe("formatPeakWindows", () => {
  it("rechnet UTC-Fenster in Sommerzeit um und fasst gleiche Tage zusammen", () => {
    expect(formatPeakWindows(deepseek, "Europe/Berlin", summer)).toBe("Mo–Fr 03:00–06:00 und 08:00–12:00")
  })
  it("rechnet in Winterzeit mit einer Stunde Versatz", () => {
    expect(formatPeakWindows(deepseek, "Europe/Berlin", winter)).toBe("Mo–Fr 02:00–05:00 und 07:00–11:00")
  })
  it("verschiebt die Tage, wenn das Fenster in Ortszeit nach Mitternacht beginnt", () => {
    expect(formatPeakWindows([{ days: "mon-fri", from: "23:00", to: "23:30" }], "Europe/Berlin", summer)).toBe(
      "Di–Sa 01:00–01:30",
    )
  })
  it("verschiebt die Tage zurück bei westlicher Zeitzone", () => {
    expect(formatPeakWindows([{ days: "mon-fri", from: "01:00", to: "04:00" }], "America/New_York", summer)).toBe(
      "So–Do 21:00–00:00",
    )
  })
  it("versteht Einzeltage, Listen und täglich", () => {
    expect(formatPeakWindows([{ days: "sat", from: "10:00", to: "11:00" }], "UTC", summer)).toBe("Sa 10:00–11:00")
    expect(formatPeakWindows([{ days: "mon,wed", from: "10:00", to: "11:00" }], "UTC", summer)).toBe(
      "Mo, Mi 10:00–11:00",
    )
    expect(formatPeakWindows([{ days: "daily", from: "10:00", to: "11:00" }], "UTC", summer)).toBe(
      "täglich 10:00–11:00",
    )
  })
  it("trennt Gruppen mit verschiedenen Tagen durch Semikolon", () => {
    expect(
      formatPeakWindows(
        [
          { days: "mon-fri", from: "10:00", to: "11:00" },
          { days: "sat-sun", from: "12:00", to: "13:00" },
        ],
        "UTC",
        summer,
      ),
    ).toBe("Mo–Fr 10:00–11:00; Sa–So 12:00–13:00")
  })
  it("lässt unverständliche Tagesangaben stehen und liefert bei leerer Liste einen leeren Text", () => {
    expect(formatPeakWindows([{ days: "feiertags", from: "10:00", to: "11:00" }], "UTC", summer)).toBe(
      "feiertags 10:00–11:00",
    )
    expect(formatPeakWindows([], "UTC", summer)).toBe("")
  })
})
