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
    [{ kind: "thinking" as const, since: "" }, "Thinking"],
    [{ kind: "writing" as const, since: "" }, "Writing"],
    [{ kind: "tool" as const, tool: "bash", since: "" }, "Running bash"],
    [{ kind: "tool" as const, since: "" }, "Running a tool"],
    [{ kind: "waiting_approval" as const, since: "" }, "Waiting for approval"],
    [{ kind: "idle" as const, since: "" }, "Waiting"],
    [{ kind: "preparing" as const, tool: "write", since: "" }, "Preparing write"],
    [{ kind: "compacting" as const, since: "" }, "Summarizing the context"],
    [{ kind: "starting" as const, since: "" }, "Starting"],
  ])("%o → %s", (a, expected) => {
    expect(formatActivity(a)).toBe(expected)
  })

  it("returns a dash without activity", () => {
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
  it("returns a dash for a missing or invalid time", () => {
    expect(formatDuration(undefined, now)).toBe("–")
    expect(formatDuration("broken", now)).toBe("–")
  })
})

describe("formatUsd", () => {
  it("formats in English with four decimal places", () => {
    expect(formatUsd(0.0123)).toBe("$0.0123")
    expect(formatUsd(0)).toBe("$0.0000")
    expect(formatUsd(1234.5)).toBe("$1,234.5000")
  })
  it("tolerates missing values", () => {
    expect(formatUsd(undefined)).toBe("–")
  })
})

describe("formatTokens", () => {
  it("adds thousands separators", () => {
    expect(formatTokens(1234567)).toBe("1,234,567")
    expect(formatTokens(undefined)).toBe("0")
  })
})

describe("formatBytes", () => {
  it.each([
    [0, "0 B"],
    [999, "999 B"],
    [2048, "2.0 KiB"],
    [5 * 1024 * 1024, "5.0 MiB"],
  ])("%d → %s", (n, expected) => {
    expect(formatBytes(n)).toBe(expected)
  })
})

describe("shortHash", () => {
  it("shortens to 12 characters", () => {
    expect(shortHash("0123456789abcdef0123")).toBe("0123456789ab…")
    expect(shortHash("abc")).toBe("abc")
  })
})

describe("formatUsage", () => {
  it("summarizes tokens and cost of a message", () => {
    expect(formatUsage({ input: 1200, output: 45, cacheRead: 300, cost: { total: 0.00123 } })).toBe(
      "1,200 in · 45 out · 300 cache · $0.0012",
    )
  })
  it("leaves out cache and cost when missing or zero", () => {
    expect(formatUsage({ input: 10, output: 5, cacheRead: 0 })).toBe("10 in · 5 out")
  })
  it("returns nothing without usage", () => {
    expect(formatUsage(undefined)).toBe("")
  })
})

describe("formatDate", () => {
  it.each([
    ["2026-09-29", "2026-09-29"],
    ["2026-01-05T23:30:00Z", "2026-01-05"],
    ["2026-01-05T00:30:00+02:00", "2026-01-05"],
  ])("%s → %s", (iso, expected) => {
    expect(formatDate(iso)).toBe(expected)
  })
  it("returns a dash for a missing or invalid date", () => {
    expect(formatDate(undefined)).toBe("–")
    expect(formatDate("yesterday")).toBe("–")
  })
})

describe("socketResultLabel", () => {
  it.each([
    ["approved", "approved"],
    ["rejected", "rejected"],
    ["expired", "expired"],
    ["Approved", "approved"],
    ["ok", "ok"],
    ["error: broken", "error: broken"],
  ])("%s → %s", (raw, expected) => {
    expect(socketResultLabel(raw)).toBe(expected)
  })
})

describe("socketOpLabel", () => {
  it.each([
    ["upload", "Upload"],
    ["list", "List"],
    ["get", "Fetch"],
    ["ping", "Ping"],
    ["internet", "Internet request"],
    ["internet_off", "Internet off"],
    ["internet_set", "Internet switch by the user"],
    ["llm", "llm"],
  ])("%s → %s", (raw, expected) => {
    expect(socketOpLabel(raw)).toBe(expected)
  })
})

describe("pricingSource", () => {
  it("takes source as the URL and retrieved as the date", () => {
    expect(pricingSource({ source: "https://x.test/prices", retrieved: "2026-09-29" })).toEqual({
      href: "https://x.test/prices",
      retrieved: "2026-09-29",
    })
  })
  it("splits the old German form \"URL, Abruf DD.MM.YYYY\"", () => {
    expect(pricingSource({ source: "https://api-docs.deepseek.com/quick_start/pricing, Abruf 29.09.2026" })).toEqual({
      href: "https://api-docs.deepseek.com/quick_start/pricing",
      retrieved: "2026-09-29",
    })
  })
  it("discards sources without an http(s) URL", () => {
    expect(pricingSource({ source: "javascript:alert(1)" })).toEqual({})
    expect(pricingSource({ retrieved: "2026-01-02" })).toEqual({ retrieved: "2026-01-02" })
    expect(pricingSource(undefined)).toEqual({})
  })
})

describe("formatUsage with tariff costs", () => {
  it("takes the orchestrator's cost over pi's flat price", () => {
    expect(formatUsage({ input: 10, output: 5, cost: { total: 0.002 } }, 0.001)).toBe("10 in · 5 out · $0.0010")
  })
  it("falls back to usage.cost.total without orchestrator cost", () => {
    expect(formatUsage({ input: 10, output: 5, cost: { total: 0.002 } })).toBe("10 in · 5 out · $0.0020")
  })
  it("does not show an orchestrator cost of 0 as missing", () => {
    expect(formatUsage({ input: 1, output: 1, cost: { total: 0.002 } }, 0)).toBe("1 in · 1 out · $0.0000")
  })
})

describe("formatUsage: provisional cost", () => {
  it("marks pi's value as provisional while the tariff value is missing", () => {
    expect(formatUsage({ input: 10, output: 5, cost: { total: 0.00123 } }, undefined, { provisional: true })).toBe(
      "10 in · 5 out · ≈ $0.0012 (provisional)",
    )
  })
  it("takes the tariff value without a mark once it is there", () => {
    expect(formatUsage({ input: 10, output: 5, cost: { total: 0.00123 } }, 0.002, { provisional: true })).toBe(
      "10 in · 5 out · $0.0020",
    )
  })
})

describe("modelPriceSource", () => {
  it("takes the source of the prices", () => {
    expect(
      modelPriceSource({
        pricing: { input: 1, output: 1, cache_read: 0, cache_write: 0, currency: "USD", source: "https://a.test", retrieved: "2026-09-29" },
        tariff: { peak_windows_utc: [], offpeak_factor: 0.5, source: "https://b.test" },
      }),
    ).toEqual({ href: "https://a.test", retrieved: "2026-09-29" })
  })
  it("falls back to the source of the tariff", () => {
    expect(
      modelPriceSource({
        pricing: { input: 1, output: 1, cache_read: 0, cache_write: 0, currency: "USD" },
        tariff: { peak_windows_utc: [], offpeak_factor: 0.5, source: "https://b.test", retrieved: "2026-09-01" },
      }),
    ).toEqual({ href: "https://b.test", retrieved: "2026-09-01" })
  })
  it("returns nothing without an http(s) source", () => {
    expect(modelPriceSource({ tariff: { peak_windows_utc: [], offpeak_factor: 1, source: "ftp://x" } })).toEqual({})
    expect(modelPriceSource({})).toEqual({})
  })
})

describe("sourceLabel", () => {
  it("composes \"Source · as of YYYY-MM-DD\"", () => {
    expect(sourceLabel({ href: "https://a", retrieved: "2026-09-29" })).toBe("Source · as of 2026-09-29")
    expect(sourceLabel({ href: "https://a" })).toBe("Source")
    expect(sourceLabel({ retrieved: "2026-09-29" })).toBe("as of 2026-09-29")
    expect(sourceLabel({})).toBe("")
  })
})

describe("hasThinkingText", () => {
  it.each([
    ["Let me think", true],
    ["", false],
    ["  \n ", false],
    ["\u200b", false],
    [undefined, false],
    [null, false],
  ])("%j → %s", (t, want) => {
    expect(hasThinkingText(t)).toBe(want)
  })
})

describe("socketOpLabel: monitoring", () => {
  it.each([
    ["agent_limit", "Limit of concurrent agents"],
    ["subagent_limit", "Subagent limit exceeded – aborted"],
    ["extension_ui", "Extension prompt declined"],
  ])("%s → %s", (op, label) => {
    expect(socketOpLabel(op)).toBe(label)
  })
})

describe("formatMs", () => {
  it.each([
    [0, "0 ms"],
    [850, "850 ms"],
    [1500, "1.5 s"],
    [65000, "1 min 5 s"],
  ])("%d → %s", (ms, s) => {
    expect(formatMs(ms)).toBe(s)
  })
})

import { isUserAbort } from "./format"

describe("isUserAbort", () => {
  it("recognises the abort via stopReason or pi's message", () => {
    expect(isUserAbort({ stopReason: "aborted" })).toBe(true)
    expect(isUserAbort({ stopReason: "error", errorMessage: "This operation was aborted" })).toBe(true)
    expect(isUserAbort({ stopReason: "error", errorMessage: "Request was aborted." })).toBe(true)
  })
  it("leaves real errors as errors", () => {
    expect(isUserAbort({ stopReason: "error", errorMessage: "429 Too Many Requests" })).toBe(false)
    expect(isUserAbort({ stopReason: "stop" })).toBe(false)
  })
})
