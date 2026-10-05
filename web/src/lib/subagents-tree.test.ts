import { describe, expect, it } from "vitest"
import type { SubagentEntry } from "@/api/types"
import {
  baseRunId,
  buildRunTree,
  groupRuns,
  pairRunEntries,
  runConfirmation,
  runStatus,
  runStatusLabel,
} from "./subagents"

const entry = (p: Partial<SubagentEntry> & Pick<SubagentEntry, "run_id" | "entry_id">): SubagentEntry => ({
  chat_id: "c",
  agent: "scout",
  kind: "text",
  payload: {},
  confirmed: false,
  created_at: "2026-09-29T10:00:00Z",
  ...p,
})
const iso = (s: string) => `2026-09-29T${s}Z`
const t = (s: string) => Date.parse(iso(s))

describe("baseRunId", () => {
  it("schneidet das Suffix paralleler Läufe ab", () => {
    expect(baseRunId("abc123#2")).toBe("abc123")
    expect(baseRunId("abc123")).toBe("abc123")
  })
})

describe("buildRunTree", () => {
  it("liefert eine leere Liste ohne Läufe", () => {
    expect(buildRunTree([])).toEqual([])
  })

  it("macht aus Einzelläufen je einen Knoten ohne Kinder, sortiert nach Start", () => {
    const runs = groupRuns([
      entry({ run_id: "b", entry_id: "1", created_at: iso("10:05:00") }),
      entry({ run_id: "a", entry_id: "1", created_at: iso("10:00:00") }),
    ])
    const tree = buildRunTree(runs)
    expect(tree.map((n) => n.id)).toEqual(["a", "b"])
    expect(tree[0].run?.runId).toBe("a")
    expect(tree[0].children).toEqual([])
  })

  it("gruppiert parallele Läufe (#n) unter ihrem Basis-Lauf, numerisch sortiert", () => {
    const runs = groupRuns([
      entry({ run_id: "p#10", entry_id: "1", created_at: iso("10:00:01") }),
      entry({ run_id: "p#2", entry_id: "1", created_at: iso("10:00:03") }),
      entry({ run_id: "p", entry_id: "1", created_at: iso("10:00:00") }),
      entry({ run_id: "q", entry_id: "1", created_at: iso("10:00:02") }),
    ])
    const tree = buildRunTree(runs)
    expect(tree.map((n) => n.id)).toEqual(["p", "q"])
    expect(tree[0].run?.runId).toBe("p")
    expect(tree[0].children.map((r) => r.runId)).toEqual(["p#2", "p#10"])
  })

  it("legt einen Gruppenknoten ohne eigenen Lauf an, wenn nur Kinder existieren", () => {
    const runs = groupRuns([
      entry({ run_id: "x#1", entry_id: "1", created_at: iso("10:00:05"), agent: "worker" }),
      entry({ run_id: "x#2", entry_id: "1", created_at: iso("10:00:04"), agent: "reviewer" }),
    ])
    const tree = buildRunTree(runs)
    expect(tree).toHaveLength(1)
    expect(tree[0]).toMatchObject({ id: "x", start: t("10:00:04"), end: t("10:00:05") })
    expect(tree[0].run).toBeUndefined()
    expect(tree[0].children.map((r) => r.runId)).toEqual(["x#1", "x#2"])
  })
})

describe("runStatus", () => {
  const now = t("10:10:00")
  const run = (last: Partial<SubagentEntry>) =>
    groupRuns([
      entry({ run_id: "r", entry_id: "1", kind: "task", created_at: iso("10:00:00"), payload: { text: "A" } }),
      entry({ run_id: "r", entry_id: "2", created_at: iso("10:09:50"), ...last }),
    ])[0]

  it("ist fertig, wenn der letzte Eintrag eine Textantwort ist", () => {
    expect(runStatus(run({ kind: "text", payload: { text: "Ergebnis" } }), { chatRunning: true, now })).toBe("done")
  })
  it("läuft, solange der Chat arbeitet und der letzte Eintrag frisch ist", () => {
    expect(runStatus(run({ kind: "tool_call", payload: { name: "bash" } }), { chatRunning: true, now })).toBe("running")
  })
  it("ist still, wenn der Chat arbeitet, aber lange nichts kam", () => {
    const r = run({ kind: "tool_call", payload: { name: "bash" } })
    expect(runStatus(r, { chatRunning: true, now: t("10:20:00") })).toBe("idle")
  })
  it("läuft ohne Zustand weiter, solange zuletzt etwas kam, auch wenn der Hauptagent ruht (Hintergrund)", () => {
    expect(runStatus(run({ kind: "tool_result", payload: { name: "bash", text: "" } }), { chatRunning: false, now })).toBe(
      "running",
    )
  })
  it("ist ohne Zustand und ohne Antwort beendet, wenn lange nichts kam und der Chat ruht", () => {
    const r = run({ kind: "tool_result", payload: { name: "bash", text: "" } })
    expect(runStatus(r, { chatRunning: false, now: t("10:20:00") })).toBe("stopped")
  })
  it("folgt dem Zustand laut pi-subagents, wenn er bekannt ist", () => {
    const meta = (state: string) => [{ chat_id: "c", run_id: "r", agent: "researcher", label: "reid", state, updated_at: iso("10:09:55") }]
    const mk = (state: string) =>
      groupRuns(
        [
          entry({ run_id: "r", entry_id: "1", kind: "task", created_at: iso("10:00:00"), payload: { text: "A" } }),
          entry({ run_id: "r", entry_id: "2", kind: "text", created_at: iso("10:00:05"), payload: { text: "Zwischenstand" } }),
        ],
        meta(state),
      )[0]
    // Text als letzter Eintrag hieße sonst „fertig“; pi-subagents sagt, er läuft noch.
    expect(runStatus(mk("running"), { chatRunning: false, now: t("10:30:00") })).toBe("running")
    expect(runStatus(mk("complete"), { chatRunning: true, now })).toBe("done")
    expect(runStatus(mk("failed"), { chatRunning: true, now })).toBe("stopped")
    expect(mk("running").label).toBe("reid")
  })
  it("zeigt einen Lauf mit Metadaten schon vor dem ersten Eintrag", () => {
    const runs = groupRuns([], [{ chat_id: "c", run_id: "x", agent: "scout", label: "daten", state: "running", started_at: iso("10:00:00"), updated_at: iso("10:00:00") }])
    expect(runs).toHaveLength(1)
    expect(runs[0]).toMatchObject({ runId: "x", agent: "scout", label: "daten" })
  })
  it("gilt als fertig mit Fehler, wenn eine Textantwort nach Fehlern kommt", () => {
    const r = groupRuns([
      entry({ run_id: "r", entry_id: "1", kind: "tool_result", payload: { is_error: true } }),
      entry({ run_id: "r", entry_id: "2", kind: "text", created_at: iso("10:00:01"), payload: { text: "ok" } }),
    ])[0]
    expect(runStatus(r, { chatRunning: false, now })).toBe("done")
  })
  it("hat deutsche Beschriftungen", () => {
    expect(runStatusLabel("running")).toBe("läuft")
    expect(runStatusLabel("done")).toBe("fertig")
    expect(runStatusLabel("idle")).toBe("still")
    expect(runStatusLabel("stopped")).toBe("ohne Antwort beendet")
  })
})

describe("runConfirmation", () => {
  it("zählt belegte Einträge laut Server oder Proxy", () => {
    const r = groupRuns([
      entry({ run_id: "r", entry_id: "1", confirmed: true }),
      entry({ run_id: "r", entry_id: "2", response_id: "resp-1" }),
      entry({ run_id: "r", entry_id: "3" }),
    ])[0]
    expect(runConfirmation(r, new Set(["resp-1"]))).toEqual({ confirmed: 2, total: 3 })
  })
})

describe("pairRunEntries", () => {
  it("setzt Aufruf und Ergebnis zu einem Werkzeug-Eintrag zusammen", () => {
    const list = [
      entry({ run_id: "r", entry_id: "t", kind: "task", payload: { text: "Auftrag" } }),
      entry({ run_id: "r", entry_id: "c1", kind: "tool_call", payload: { name: "bash", arguments: '{"command":"ls"}' } }),
      entry({ run_id: "r", entry_id: "c2", kind: "tool_call", payload: { name: "read", arguments: "{}" } }),
      entry({ run_id: "r", entry_id: "r2", kind: "tool_result", payload: { name: "read", text: "Inhalt" } }),
      entry({ run_id: "r", entry_id: "r1", kind: "tool_result", payload: { name: "bash", text: "a b" } }),
      entry({ run_id: "r", entry_id: "x", kind: "text", payload: { text: "fertig" } }),
    ]
    const items = pairRunEntries(list)
    expect(items.map((i) => i.type)).toEqual(["task", "tool", "tool", "text"])
    const [, bash, read] = items
    expect(bash.type === "tool" && [bash.call?.entry_id, bash.result?.entry_id]).toEqual(["c1", "r1"])
    expect(read.type === "tool" && [read.call?.entry_id, read.result?.entry_id]).toEqual(["c2", "r2"])
  })

  it("ordnet ein Ergebnis ohne Namen dem ältesten offenen Aufruf zu", () => {
    const items = pairRunEntries([
      entry({ run_id: "r", entry_id: "c1", kind: "tool_call", payload: { name: "bash" } }),
      entry({ run_id: "r", entry_id: "r1", kind: "tool_result", payload: { text: "x" } }),
    ])
    expect(items).toHaveLength(1)
    expect(items[0].type === "tool" && items[0].result?.entry_id).toBe("r1")
  })

  it("zeigt ein Ergebnis ohne passenden Aufruf als eigenen Eintrag", () => {
    const items = pairRunEntries([entry({ run_id: "r", entry_id: "r1", kind: "tool_result", payload: { name: "bash" } })])
    expect(items).toHaveLength(1)
    expect(items[0].type === "tool" && items[0].call).toBeUndefined()
    expect(items[0].type === "tool" && items[0].result?.entry_id).toBe("r1")
  })

  it("lässt einen offenen Aufruf ohne Ergebnis stehen", () => {
    const items = pairRunEntries([entry({ run_id: "r", entry_id: "c1", kind: "tool_call", payload: { name: "bash" } })])
    expect(items[0].type === "tool" && items[0].result).toBeUndefined()
  })
})

describe("pairRunEntries mit Aufruf-ID", () => {
  it("ordnet parallele gleichnamige Aufrufe über die ID zu", () => {
    const e = (kind: SubagentEntry["kind"], payload: SubagentEntry["payload"], id: string): SubagentEntry => ({
      chat_id: "c", run_id: "r", entry_id: id, agent: "scout", kind, payload, confirmed: false, created_at: "2026-09-29T12:00:00Z",
    })
    const items = pairRunEntries([
      e("tool_call", { name: "bash", arguments: '{"command":"a"}', id: "c1" }, "1"),
      e("tool_call", { name: "bash", arguments: '{"command":"b"}', id: "c2" }, "2"),
      e("tool_result", { name: "bash", text: "B", tool_call_id: "c2" }, "3"),
      e("tool_result", { name: "bash", text: "A", tool_call_id: "c1" }, "4"),
    ])
    const tools = items.filter((i) => i.type === "tool")
    expect(tools.map((t) => [t.call?.payload?.arguments, t.result?.payload?.text])).toEqual([
      ['{"command":"a"}', "A"],
      ['{"command":"b"}', "B"],
    ])
  })
})
