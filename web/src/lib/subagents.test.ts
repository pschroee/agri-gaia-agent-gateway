import { describe, expect, it } from "vitest"
import type { SocketCall, SubagentEntry } from "@/api/types"
import {
  assignRuns,
  clipText,
  effectiveTimes,
  groupRuns,
  isEntryConfirmed,
  limitErrorKind,
  limitNotices,
  mergeSubagentEntries,
  parseArguments,
  placeAfter,
  runLabel,
  shortRunId,
  subagentLimitLabel,
  toolAgents,
} from "./subagents"
import type { TranscriptItem } from "./stream"

const entry = (p: Partial<SubagentEntry> & Pick<SubagentEntry, "run_id" | "entry_id">): SubagentEntry => ({
  chat_id: "c",
  agent: "scout",
  kind: "text",
  payload: {},
  confirmed: false,
  created_at: "2026-09-29T10:00:00Z",
  ...p,
})
const t = (s: string) => Date.parse(`2026-09-29T${s}Z`)
const iso = (s: string) => `2026-09-29T${s}Z`

describe("mergeSubagentEntries", () => {
  it("hängt neue Einträge an und ersetzt gleiche (run_id, entry_id)", () => {
    const a = entry({ run_id: "r1", entry_id: "e1" })
    const b = entry({ run_id: "r1", entry_id: "e2" })
    const a2 = { ...a, confirmed: true }
    expect(mergeSubagentEntries([a], [b, a2])).toEqual([a2, b])
  })
  it("unterscheidet gleiche entry_id in verschiedenen Läufen", () => {
    const a = entry({ run_id: "r1", entry_id: "e1" })
    const b = entry({ run_id: "r2", entry_id: "e1" })
    expect(mergeSubagentEntries([a], [b])).toHaveLength(2)
  })
})

describe("groupRuns", () => {
  it("gruppiert je run_id, sortiert nach Start und zählt", () => {
    const runs = groupRuns([
      entry({ run_id: "r2", entry_id: "x", created_at: iso("10:05:00"), agent: "worker", kind: "task", payload: { text: "B" } }),
      entry({ run_id: "r1", entry_id: "a", created_at: iso("10:00:00"), kind: "task", payload: { text: "Finde X" } }),
      entry({ run_id: "r1", entry_id: "b", created_at: iso("10:00:02"), kind: "tool_call", payload: { name: "bash", arguments: "{}" } }),
      entry({ run_id: "r1", entry_id: "c", created_at: iso("10:00:03"), kind: "tool_result", payload: { name: "bash", text: "err", is_error: true } }),
    ])
    expect(runs.map((r) => r.runId)).toEqual(["r1", "r2"])
    expect(runs[0]).toMatchObject({ agent: "scout", task: "Finde X", toolCalls: 1, errors: 1, start: t("10:00:00"), end: t("10:00:03") })
    expect(runs[0].entries.map((e) => e.entry_id)).toEqual(["a", "b", "c"])
    expect(runs[1].agent).toBe("worker")
  })
  it("liefert eine leere Liste ohne Einträge", () => {
    expect(groupRuns([])).toEqual([])
  })
})

describe("shortRunId und runLabel", () => {
  it("kürzt eine UUID auf sechs Zeichen und behält den Index paralleler Kinder", () => {
    expect(shortRunId("abc123de-0000-4000-8000-000000000000")).toBe("abc123")
    expect(shortRunId("abc123de-0000-4000-8000-000000000000#2")).toBe("abc123#2")
    expect(shortRunId("kurz")).toBe("kurz")
  })
  it("baut die Überschrift", () => {
    expect(runLabel({ agent: "scout", runId: "abc123de-0000" })).toBe("Subagent scout · Lauf abc123")
    expect(runLabel({ agent: "", runId: "abc123de-0000" })).toBe("Subagent · Lauf abc123")
  })
})

describe("isEntryConfirmed", () => {
  it("nimmt confirmed vom Server oder eine am Proxy bekannte response_id", () => {
    expect(isEntryConfirmed(entry({ run_id: "r", entry_id: "e", confirmed: true }), new Set())).toBe(true)
    expect(isEntryConfirmed(entry({ run_id: "r", entry_id: "e", response_id: "resp1" }), new Set(["resp1"]))).toBe(true)
    expect(isEntryConfirmed(entry({ run_id: "r", entry_id: "e", response_id: "resp1" }), new Set(["x"]))).toBe(false)
    expect(isEntryConfirmed(entry({ run_id: "r", entry_id: "e" }), new Set([""]))).toBe(false)
  })
})

describe("parseArguments", () => {
  it("liest JSON und fällt sonst auf den Text zurück", () => {
    expect(parseArguments('{"command":"ls"}')).toEqual({ command: "ls" })
    expect(parseArguments("kein json")).toBe("kein json")
    expect(parseArguments(undefined)).toBeUndefined()
    expect(parseArguments("")).toBeUndefined()
  })
})

describe("clipText", () => {
  it("kürzt lange Texte und meldet es", () => {
    expect(clipText("abc", 10)).toEqual({ text: "abc", clipped: false })
    expect(clipText("abcdefghijkl", 5)).toEqual({ text: "abcde …", clipped: true })
  })
})

describe("toolAgents", () => {
  it("findet Agentennamen in einfachen und parallelen Aufrufen", () => {
    expect(toolAgents({ agent: "scout", task: "x" })).toEqual(["scout"])
    expect(toolAgents({ tasks: [{ agent: "a" }, { agent: "b" }] })).toEqual(["a", "b"])
    expect(toolAgents({ chain: [{ agent: "c" }] })).toEqual(["c"])
    expect(toolAgents("x")).toEqual([])
  })
})

describe("effectiveTimes und placeAfter", () => {
  const items: TranscriptItem[] = [
    { kind: "user", key: "u1", text: "a", time: t("10:00:00") },
    { kind: "assistant", key: "a1", blocks: [], streaming: false, time: t("10:00:10") },
    { kind: "assistant", key: "a2", blocks: [], streaming: true },
  ]
  it("erbt fehlende Zeiten vom Vorgänger", () => {
    expect(effectiveTimes(items)).toEqual([t("10:00:00"), t("10:00:10"), t("10:00:10")])
  })
  it("setzt hinter den letzten Eintrag, der nicht später liegt", () => {
    expect(placeAfter(items, t("09:59:00"))).toBe(-1)
    expect(placeAfter(items, t("10:00:05"))).toBe(0)
    expect(placeAfter(items, t("10:00:20"))).toBe(2)
  })
})

describe("assignRuns", () => {
  const call = (id: string, args: unknown) => ({ type: "toolCall" as const, id, name: "subagent", arguments: args })
  const items: TranscriptItem[] = [
    { kind: "user", key: "u1", text: "los", time: t("10:00:00") },
    {
      kind: "assistant",
      key: "a1",
      blocks: [call("tc1", { agent: "scout", task: "A" }), call("tc2", { agent: "worker", task: "B" })],
      streaming: false,
      time: t("10:00:05"),
    },
    { kind: "assistant", key: "a2", blocks: [{ type: "text", text: "fertig" }], streaming: false, time: t("10:01:00") },
    { kind: "assistant", key: "a3", blocks: [call("tc3", { agent: "scout" })], streaming: false, time: t("10:02:00") },
  ]
  const run = (runId: string, agent: string, at: string) => ({ runId, agent, start: t(at) })

  it("ordnet einen Lauf dem letzten subagent-Aufruf davor zu, bevorzugt mit passendem Agenten", () => {
    const r = assignRuns(items, [run("r1", "worker", "10:00:07"), run("r2", "scout", "10:00:08"), run("r3", "scout", "10:02:03")])
    expect(r.byTool).toEqual({ tc2: ["r1"], tc1: ["r2"], tc3: ["r3"] })
    expect(r.loose).toEqual({})
  })
  it("nimmt ohne passenden Agenten den ersten Aufruf der Nachricht", () => {
    const r = assignRuns(items, [run("r1", "fremd", "10:00:30")])
    expect(r.byTool).toEqual({ tc1: ["r1"] })
  })
  it("setzt Läufe vor jedem subagent-Aufruf lose nach Zeit in den Verlauf", () => {
    const r = assignRuns(items, [run("r0", "scout", "10:00:01")])
    expect(r.byTool).toEqual({})
    expect(r.loose).toEqual({ 0: ["r0"] })
  })
})

describe("limitErrorKind", () => {
  it("erkennt die beiden Grenzmeldungen des Orchestrators", () => {
    expect(limitErrorKind("Modellaufruf abgewiesen: höchstens 3 gleichzeitige Agenten (Hauptagent und 2 Subagenten) erlaubt")).toBe(
      "agent_limit",
    )
    expect(limitErrorKind("Grenze überschritten: 3 Subagenten gestartet, erlaubt sind 2. Der Durchgang wurde abgebrochen.")).toBe(
      "subagent_limit",
    )
    expect(limitErrorKind("Kompaktierung fehlgeschlagen: x")).toBeUndefined()
    expect(limitErrorKind(undefined)).toBeUndefined()
  })
})

describe("limitNotices", () => {
  const sc = (id: number, op: string, detail: string, at: string): SocketCall => ({
    id,
    slot_id: "p",
    via: "proxy",
    op,
    detail,
    result: "abgewiesen",
    created_at: iso(at),
  })
  it("macht aus Grenz-Einträgen Hinweise und fasst direkt aufeinanderfolgende gleiche zusammen", () => {
    const n = limitNotices([
      sc(1, "upload", "x", "10:00:00"),
      sc(2, "agent_limit", "höchstens 3 gleichzeitige Agenten", "10:00:01"),
      sc(3, "agent_limit", "höchstens 3 gleichzeitige Agenten", "10:00:02"),
      sc(4, "subagent_limit", "3 gestartet, 2 erlaubt", "10:00:03"),
    ])
    expect(n).toEqual([
      {
        id: 2,
        op: "agent_limit",
        time: t("10:00:01"),
        count: 2,
        text: "Grenze gleichzeitiger Agenten erreicht: Modellaufruf am Proxy abgewiesen (höchstens 3 gleichzeitige Agenten)",
      },
      {
        id: 4,
        op: "subagent_limit",
        time: t("10:00:03"),
        count: 1,
        text: "Subagenten-Grenze überschritten – abgebrochen (3 gestartet, 2 erlaubt)",
      },
    ])
  })
})

describe("subagentLimitLabel", () => {
  it("zeigt gestartete und erlaubte Subagenten", () => {
    expect(subagentLimitLabel({ subagents: 1, max_subagents: 2 })).toBe("Subagenten 1 / 2")
    expect(subagentLimitLabel({})).toBe("Subagenten 0 / –")
  })
})
