import { describe, expect, it } from "vitest"
import { DELEGATION_TEMPLATES, countViolations, delegationFrom } from "./delegationTemplates"

const byId = (id: string) => DELEGATION_TEMPLATES.find((t) => t.id === id)

describe("delegationFrom", () => {
  const now = new Date("2026-10-05T10:00:00Z")
  it("ohne Delegation liefert nichts", () => {
    expect(delegationFrom(byId("none"), 8, now)).toBeUndefined()
  })
  it("nur lesen: alle Ressourcen mit *, Ablauf und enforce", () => {
    const d = delegationFrom(byId("read"), 8, now)!
    expect(d.enforce).toBe(true)
    expect(d.expires_at).toBe("2026-10-05T18:00:00.000Z")
    expect(d.rules.every((r) => r.action === "read" && r.ids?.[0] === "*")).toBe(true)
    expect(d.rules.map((r) => r.resource)).toContain("container_image")
    expect(d.rules.some((r) => r.resource === "api")).toBe(false)
  })
  it("Training: eigene Datensätze nur über own, Start ohne own", () => {
    const d = delegationFrom(byId("train-own"), 1, now)!
    expect(d.rules).toContainEqual({ action: "update", resource: "dataset", ids: ["own"] })
    expect(d.rules).toContainEqual({ action: "create", resource: "dataset", ids: undefined })
    expect(d.rules).toContainEqual({ action: "run", resource: "training", ids: ["*"] })
  })
  it("kopiert die Regeln", () => {
    const d = delegationFrom(byId("read"), 1, now)!
    d.rules[0].ids!.push("x")
    expect(byId("read")!.rules![0].ids).toEqual(["*"])
  })
})

describe("countViolations", () => {
  it("zählt Übergriffe unabhängig von Groß-/Kleinschreibung", () => {
    expect(
      countViolations([
        { result: "übergriff abgewiesen: dataset 7" },
        { result: "ok 200 · Übergriff, nur protokolliert: x" },
        { result: "ok 200" },
        {},
      ]),
    ).toBe(2)
  })
})
