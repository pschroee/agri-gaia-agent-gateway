import { describe, expect, it } from "vitest"
import { DELEGATION_TEMPLATES, countViolations, delegationFrom, isViolation } from "./delegationTemplates"

const byId = (id: string) => DELEGATION_TEMPLATES.find((t) => t.id === id)

describe("delegationFrom", () => {
  const now = new Date("2026-10-05T10:00:00Z")
  it("no delegation yields nothing", () => {
    expect(delegationFrom(byId("none"), 8, now)).toBeUndefined()
  })
  it("read only: all resources with *, expiry and enforce", () => {
    const d = delegationFrom(byId("read"), 8, now)!
    expect(d.enforce).toBe(true)
    expect(d.expires_at).toBe("2026-10-05T18:00:00.000Z")
    expect(d.rules.every((r) => r.action === "read" && r.ids?.[0] === "*")).toBe(true)
    expect(d.rules.map((r) => r.resource)).toContain("container_image")
    expect(d.rules.some((r) => r.resource === "api")).toBe(false)
  })
  it("training: own datasets only via own, start without own", () => {
    const d = delegationFrom(byId("train-own"), 1, now)!
    expect(d.rules).toContainEqual({ action: "update", resource: "dataset", ids: ["own"] })
    expect(d.rules).toContainEqual({ action: "create", resource: "dataset", ids: undefined })
    expect(d.rules).toContainEqual({ action: "run", resource: "training", ids: ["*"] })
  })
  it("copies the rules", () => {
    const d = delegationFrom(byId("read"), 1, now)!
    d.rules[0].ids!.push("x")
    expect(byId("read")!.rules![0].ids).toEqual(["*"])
  })
})

describe("countViolations", () => {
  it("counts violations regardless of case", () => {
    expect(
      countViolations([
        { result: "violation blocked: dataset 7" },
        { result: "ok 200 · Violation, logged only: x" },
        { result: "ok 200" },
        {},
      ]),
    ).toBe(2)
  })
  it("isViolation", () => {
    expect(isViolation("violation blocked: x")).toBe(true)
    expect(isViolation("refused: no rule")).toBe(false)
    expect(isViolation(undefined)).toBe(false)
  })
})
