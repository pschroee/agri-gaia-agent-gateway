import { describe, expect, it } from "vitest"
import {
  formatActivity,
  formatBytes,
  formatDate,
  pricingSource,
  modelPriceSource,
  sourceLabel,
  hasThinkingText,
  formatDuration,
  formatTokens,
  formatUsage,
  formatUsd,
  shortHash,
  socketOpLabel,
  socketResultLabel,
  formatMs,
} from "./format"

describe("formatActivity", () => {
  it.each([
    [{ kind: "thinking" as const, since: "" }, "Denkt"],
    [{ kind: "writing" as const, since: "" }, "Schreibt"],
    [{ kind: "tool" as const, tool: "bash", since: "" }, "Führt bash aus"],
    [{ kind: "tool" as const, since: "" }, "Führt ein Werkzeug aus"],
    [{ kind: "waiting_approval" as const, since: "" }, "Wartet auf Bestätigung"],
    [{ kind: "idle" as const, since: "" }, "Wartet"],
    [{ kind: "preparing" as const, tool: "write", since: "" }, "Bereitet write vor"],
    [{ kind: "compacting" as const, since: "" }, "Fasst den Kontext zusammen"],
    [{ kind: "starting" as const, since: "" }, "Startet"],
  ])("%o → %s", (a, expected) => {
    expect(formatActivity(a)).toBe(expected)
  })

  it("liefert einen Strich ohne Tätigkeit", () => {
    expect(formatActivity(undefined)).toBe("–")
  })
})

describe("formatDuration", () => {
  const now = new Date("2026-09-29T10:00:00Z").getTime()
  it.each([
    ["2026-09-29T10:00:00Z", "0 s"],
    ["2026-09-29T09:59:48Z", "12 s"],
    ["2026-09-29T09:56:56Z", "3 min 4 s"],
    ["2026-09-29T08:58:00Z", "1 h 2 min"],
    ["2026-09-29T10:00:05Z", "0 s"],
  ])("%s → %s", (since, expected) => {
    expect(formatDuration(since, now)).toBe(expected)
  })
  it("liefert einen Strich bei fehlender oder ungültiger Zeit", () => {
    expect(formatDuration(undefined, now)).toBe("–")
    expect(formatDuration("kaputt", now)).toBe("–")
  })
})

describe("formatUsd", () => {
  it("formatiert deutsch mit vier Nachkommastellen", () => {
    expect(formatUsd(0.0123)).toBe("$0,0123")
    expect(formatUsd(0)).toBe("$0,0000")
    expect(formatUsd(1234.5)).toBe("$1.234,5000")
  })
  it("verträgt fehlende Werte", () => {
    expect(formatUsd(undefined)).toBe("–")
  })
})

describe("formatTokens", () => {
  it("setzt Tausenderpunkte", () => {
    expect(formatTokens(1234567)).toBe("1.234.567")
    expect(formatTokens(undefined)).toBe("0")
  })
})

describe("formatBytes", () => {
  it.each([
    [0, "0 B"],
    [999, "999 B"],
    [2048, "2,0 KiB"],
    [5 * 1024 * 1024, "5,0 MiB"],
  ])("%d → %s", (n, expected) => {
    expect(formatBytes(n)).toBe(expected)
  })
})

describe("shortHash", () => {
  it("kürzt auf 12 Zeichen", () => {
    expect(shortHash("0123456789abcdef0123")).toBe("0123456789ab…")
    expect(shortHash("abc")).toBe("abc")
  })
})

describe("formatUsage", () => {
  it("fasst Tokens und Kosten einer Nachricht zusammen", () => {
    expect(formatUsage({ input: 1200, output: 45, cacheRead: 300, cost: { total: 0.00123 } })).toBe(
      "1.200 ein · 45 aus · 300 Cache · $0,0012",
    )
  })
  it("lässt Cache und Kosten weg, wenn sie fehlen oder null sind", () => {
    expect(formatUsage({ input: 10, output: 5, cacheRead: 0 })).toBe("10 ein · 5 aus")
  })
  it("liefert nichts ohne usage", () => {
    expect(formatUsage(undefined)).toBe("")
  })
})

describe("formatDate", () => {
  it.each([
    ["2026-09-29", "29.09.2026"],
    ["2026-01-05T23:30:00Z", "05.01.2026"],
    ["2026-01-05T00:30:00+02:00", "05.01.2026"],
  ])("%s → %s", (iso, expected) => {
    expect(formatDate(iso)).toBe(expected)
  })
  it("liefert einen Strich bei fehlendem oder ungültigem Datum", () => {
    expect(formatDate(undefined)).toBe("–")
    expect(formatDate("gestern")).toBe("–")
  })
})

describe("socketResultLabel", () => {
  it.each([
    ["approved", "bestätigt"],
    ["rejected", "abgelehnt"],
    ["expired", "abgelaufen"],
    ["Approved", "bestätigt"],
    ["ok", "ok"],
    ["error: kaputt", "error: kaputt"],
  ])("%s → %s", (raw, expected) => {
    expect(socketResultLabel(raw)).toBe(expected)
  })
})

describe("socketOpLabel", () => {
  it.each([
    ["upload", "Upload"],
    ["list", "Liste"],
    ["get", "Abruf"],
    ["ping", "Ping"],
    ["llm", "llm"],
  ])("%s → %s", (raw, expected) => {
    expect(socketOpLabel(raw)).toBe(expected)
  })
})

describe("pricingSource", () => {
  it("nimmt source als URL und retrieved als Stand", () => {
    expect(pricingSource({ source: "https://x.test/preise", retrieved: "2026-09-29" })).toEqual({
      href: "https://x.test/preise",
      retrieved: "29.09.2026",
    })
  })
  it("trennt die Altform „URL, Abruf TT.MM.JJJJ“ auf", () => {
    expect(pricingSource({ source: "https://api-docs.deepseek.com/quick_start/pricing, Abruf 29.09.2026" })).toEqual({
      href: "https://api-docs.deepseek.com/quick_start/pricing",
      retrieved: "29.09.2026",
    })
  })
  it("verwirft Quellen ohne http(s)-URL", () => {
    expect(pricingSource({ source: "javascript:alert(1)" })).toEqual({})
    expect(pricingSource({ retrieved: "2026-01-02" })).toEqual({ retrieved: "02.01.2026" })
    expect(pricingSource(undefined)).toEqual({})
  })
})

describe("formatUsage mit Tarifkosten", () => {
  it("nimmt die Kosten des Orchestrators vor pis Einheitspreis", () => {
    expect(formatUsage({ input: 10, output: 5, cost: { total: 0.002 } }, 0.001)).toBe("10 ein · 5 aus · $0,0010")
  })
  it("fällt ohne Orchestrator-Kosten auf usage.cost.total zurück", () => {
    expect(formatUsage({ input: 10, output: 5, cost: { total: 0.002 } })).toBe("10 ein · 5 aus · $0,0020")
  })
  it("zeigt Kosten 0 des Orchestrators nicht als fehlend", () => {
    expect(formatUsage({ input: 1, output: 1, cost: { total: 0.002 } }, 0)).toBe("1 ein · 1 aus · $0,0000")
  })
})

describe("formatUsage: vorläufige Kosten", () => {
  it("kennzeichnet pis Wert als vorläufig, solange der Tarifwert fehlt", () => {
    expect(formatUsage({ input: 10, output: 5, cost: { total: 0.00123 } }, undefined, { provisional: true })).toBe(
      "10 ein · 5 aus · ≈ $0,0012 (vorläufig)",
    )
  })
  it("nimmt den Tarifwert ohne Kennzeichnung, sobald er da ist", () => {
    expect(formatUsage({ input: 10, output: 5, cost: { total: 0.00123 } }, 0.002, { provisional: true })).toBe(
      "10 ein · 5 aus · $0,0020",
    )
  })
})

describe("modelPriceSource", () => {
  it("nimmt die Quelle der Preise", () => {
    expect(
      modelPriceSource({
        pricing: { input: 1, output: 1, cache_read: 0, cache_write: 0, currency: "USD", source: "https://a.test", retrieved: "2026-09-29" },
        tariff: { peak_windows_utc: [], offpeak_factor: 0.5, source: "https://b.test" },
      }),
    ).toEqual({ href: "https://a.test", retrieved: "29.09.2026" })
  })
  it("fällt auf die Quelle des Tarifs zurück", () => {
    expect(
      modelPriceSource({
        pricing: { input: 1, output: 1, cache_read: 0, cache_write: 0, currency: "USD" },
        tariff: { peak_windows_utc: [], offpeak_factor: 0.5, source: "https://b.test", retrieved: "2026-09-01" },
      }),
    ).toEqual({ href: "https://b.test", retrieved: "01.09.2026" })
  })
  it("liefert nichts ohne http(s)-Quelle", () => {
    expect(modelPriceSource({ tariff: { peak_windows_utc: [], offpeak_factor: 1, source: "ftp://x" } })).toEqual({})
    expect(modelPriceSource({})).toEqual({})
  })
})

describe("sourceLabel", () => {
  it("setzt „Quelle · Stand TT.MM.JJJJ“ zusammen", () => {
    expect(sourceLabel({ href: "https://a", retrieved: "29.09.2026" })).toBe("Quelle · Stand 29.09.2026")
    expect(sourceLabel({ href: "https://a" })).toBe("Quelle")
    expect(sourceLabel({ retrieved: "29.09.2026" })).toBe("Stand 29.09.2026")
    expect(sourceLabel({})).toBe("")
  })
})

describe("hasThinkingText", () => {
  it.each([
    ["Ich überlege", true],
    ["", false],
    ["  \n ", false],
    ["\u200b", false],
    [undefined, false],
    [null, false],
  ])("%j → %s", (t, want) => {
    expect(hasThinkingText(t)).toBe(want)
  })
})

describe("socketOpLabel: Überwachung", () => {
  it.each([
    ["agent_limit", "Grenze gleichzeitiger Agenten"],
    ["subagent_limit", "Subagenten-Grenze überschritten – abgebrochen"],
    ["extension_ui", "Rückfrage einer Extension abgelehnt"],
  ])("%s → %s", (op, label) => {
    expect(socketOpLabel(op)).toBe(label)
  })
})

describe("formatMs", () => {
  it.each([
    [0, "0 ms"],
    [850, "850 ms"],
    [1500, "1,5 s"],
    [65000, "1 min 5 s"],
  ])("%d → %s", (ms, s) => {
    expect(formatMs(ms)).toBe(s)
  })
})

import { isUserAbort } from "./format"

describe("isUserAbort", () => {
  it("erkennt den Abbruch über stopReason oder pis Meldung", () => {
    expect(isUserAbort({ stopReason: "aborted" })).toBe(true)
    expect(isUserAbort({ stopReason: "error", errorMessage: "This operation was aborted" })).toBe(true)
    expect(isUserAbort({ stopReason: "error", errorMessage: "Request was aborted." })).toBe(true)
  })
  it("lässt echte Fehler als Fehler stehen", () => {
    expect(isUserAbort({ stopReason: "error", errorMessage: "429 Too Many Requests" })).toBe(false)
    expect(isUserAbort({ stopReason: "stop" })).toBe(false)
  })
})
