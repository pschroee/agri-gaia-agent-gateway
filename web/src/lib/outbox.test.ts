// Gesendete Nachrichten (optimistisch), Fortsetzen in frischer Sandbox und Übergabe der
// Warteschlange im Verlauf (lib/stream), dazu die Anzeige (lib/resume, lib/queue).
import { describe, expect, it } from "vitest"
import type { PiEvent, ResumeStep, StoredMessage } from "@/api/types"
import { autoHeldText, expectQueued, holdReasonText, queuePreview, queueRows } from "./queue"
import { resumeSummary, stepDetail } from "./resume"
import {
  addPending,
  applyPiEvent,
  applyQueueDelivered,
  applyResumeStep,
  awaitingAnswer,
  dropPending,
  emptyTranscript,
  failPending,
  hydrate,
  type ResumeItem,
  type TranscriptState,
} from "./stream"

const userEnd = (text: string): PiEvent => ({ type: "message_end", message: { role: "user", content: [{ type: "text", text }] } })
const assistantStart: PiEvent = { type: "message_start", message: { role: "assistant", content: [] } }
const step = (phase: ResumeStep["phase"], status: ResumeStep["status"], rest: Partial<ResumeStep> = {}): ResumeStep => ({
  id: "r1",
  phase,
  status,
  at: "2026-09-29T10:00:00Z",
  ...rest,
})
const stored = (seq: number, role: string, text: string): StoredMessage => ({
  seq,
  role,
  message: { role, content: [{ type: "text", text }] },
  created_at: "2026-09-29T10:00:00Z",
})
const kinds = (s: TranscriptState) => s.items.map((i) => i.kind)

/** Vollständiges Fortsetzen: alle Schritte fertig, dann ready. */
const fullResume = (s: TranscriptState): TranscriptState =>
  [
    step("acquire", "running"),
    step("acquire", "done", { ms: 120, detail: "p-1" }),
    step("session", "running"),
    step("session", "done", { ms: 300, size: 2048 }),
    step("settings", "running"),
    step("settings", "done", { detail: "Internet aus" }),
    step("workspace", "running"),
    step("workspace", "done", { size: 1258291, files: 14 }),
    step("inputs", "running"),
    step("inputs", "done", { size: 0, files: 0 }),
    step("ready", "done", { ms: 3400 }),
  ].reduce(applyResumeStep, s)

describe("gesendete Nachricht (optimistisch)", () => {
  it("steht sofort da und wird von pis Nutzernachricht ohne Doppelung ersetzt", () => {
    let s = addPending(emptyTranscript(), "p1", "Hallo")
    expect(s.pending.map((p) => p.text)).toEqual(["Hallo"])
    s = applyPiEvent(s, userEnd("Hallo"))
    expect(s.pending).toEqual([])
    expect(kinds(s)).toEqual(["user"])
  })

  it("ersetzt die älteste, wenn pi den Text verändert (Skill, Vorlage)", () => {
    let s = addPending(emptyTranscript(), "p1", "/skill:bericht")
    s = addPending(s, "p2", "danach")
    s = applyPiEvent(s, userEnd("<skill>…</skill>"))
    expect(s.pending.map((p) => p.key)).toEqual(["p2"])
  })

  it("behält Anhänge im Text (dieselbe Form, die der Server anhängt)", () => {
    const text = "Schau\n\n[Anhänge unter /workspace/inputs/]\n- a.csv"
    const s = applyPiEvent(addPending(emptyTranscript(), "p1", text), userEnd(text))
    expect(s.pending).toEqual([])
    expect(s.items[0].kind === "user" && s.items[0].text).toBe(text)
  })

  it("bleibt bei einem Fehler als nicht gesendet stehen und verschwindet mit der nächsten Sendung", () => {
    let s = failPending(addPending(emptyTranscript(), "p1", "eins"), "p1")
    expect(s.pending[0].failed).toBe(true)
    // pis Nutzernachricht ersetzt keine gescheiterte
    s = addPending(s, "p2", "eins")
    expect(s.pending.map((p) => p.key)).toEqual(["p2"])
  })

  it("lässt sich entfernen", () => {
    expect(dropPending(addPending(emptyTranscript(), "p1", "x"), "p1").pending).toEqual([])
  })

  it("überlebt das Neuladen, bis die Historie sie enthält", () => {
    let s = hydrate(emptyTranscript(), [stored(1, "user", "alt"), stored(2, "assistant", "ok")])
    s = addPending(s, "p1", "neu")
    expect(s.pending[0].afterSeq).toBe(2)
    // Historie noch ohne die neue Nachricht: bleibt stehen
    s = hydrate(s, [stored(1, "user", "alt"), stored(2, "assistant", "ok")])
    expect(s.pending).toHaveLength(1)
    // gleicher Text vor dem Anker zählt nicht
    s = hydrate(s, [stored(1, "user", "neu"), stored(2, "assistant", "ok")])
    expect(s.pending).toHaveLength(1)
    s = hydrate(s, [stored(1, "user", "alt"), stored(2, "assistant", "ok"), stored(3, "user", "neu")])
    expect(s.pending).toEqual([])
    expect(kinds(s)).toEqual(["user", "assistant", "user"])
  })
})

describe("Fortsetzen in frischer Sandbox", () => {
  it("zeigt alle Schritte von Anfang an, den laufenden als laufend", () => {
    let s = addPending(emptyTranscript(), "p1", "weiter")
    s = applyResumeStep(s, step("acquire", "running"))
    const r = s.resume!
    expect(r.state).toBe("running")
    expect(r.steps.map((x) => `${x.phase}:${x.status}`)).toEqual([
      "acquire:running",
      "session:pending",
      "settings:pending",
      "workspace:pending",
      "inputs:pending",
    ])
    expect(resumeSummary(r)).toBe("Chat wird fortgesetzt …")
    // Solange fortgesetzt wird, kein „Denkt …“
    expect(awaitingAnswer(s)).toBe(false)
  })

  it("übernimmt Dauer, Größe und Dateizahl und klappt nach ready auf eine Zeile zusammen", () => {
    const s = fullResume(addPending(emptyTranscript(), "p1", "weiter"))
    const r = s.resume!
    expect(r.state).toBe("done")
    expect(resumeSummary(r)).toBe("Fortgesetzt in frischer Sandbox · 3,4 s")
    const ws = r.steps.find((x) => x.phase === "workspace")!
    expect(stepDetail(ws)).toBe("1,2 MiB, 14 Dateien")
    expect(stepDetail(r.steps.find((x) => x.phase === "inputs")!)).toBe("keine Dateien")
    expect(stepDetail(r.steps.find((x) => x.phase === "acquire")!)).toBeUndefined()
    expect(stepDetail(r.steps.find((x) => x.phase === "session")!)).toBe("2,0 KiB")
    expect(awaitingAnswer(s)).toBe(true)
  })

  it("steht nach der Nutzernachricht im Verlauf, danach folgt die Antwort", () => {
    let s = fullResume(addPending(emptyTranscript(), "p1", "weiter"))
    s = applyPiEvent(s, { type: "agent_start" })
    s = applyPiEvent(s, userEnd("weiter"))
    expect(s.resume).toBeUndefined()
    expect(s.pending).toEqual([])
    expect(kinds(s)).toEqual(["user", "resume"])
    s = applyPiEvent(s, assistantStart)
    expect(kinds(s)).toEqual(["user", "resume", "assistant"])
  })

  it("wandert vor den nächsten Eintrag, wenn keine Nutzernachricht folgt (/compact)", () => {
    let s = fullResume(emptyTranscript())
    s = applyPiEvent(s, { type: "compaction_start", reason: "manual" })
    expect(kinds(s)).toEqual(["resume", "compaction"])
  })

  it("bleibt beim Neuladen hinter der Nutzernachricht stehen", () => {
    let s = hydrate(emptyTranscript(), [stored(1, "user", "a"), stored(2, "assistant", "b")])
    s = fullResume(addPending(s, "p1", "weiter"))
    s = applyPiEvent(s, userEnd("weiter"))
    s = applyPiEvent(s, assistantStart)
    s = applyPiEvent(s, { type: "message_end", message: { role: "assistant", content: [{ type: "text", text: "c" }] } })
    s = hydrate(s, [stored(1, "user", "a"), stored(2, "assistant", "b"), stored(3, "user", "weiter"), stored(4, "assistant", "c")])
    expect(kinds(s)).toEqual(["user", "assistant", "user", "resume", "assistant"])
    const r = s.items[3] as ResumeItem
    expect(r.state).toBe("done")
  })

  it("zeigt bei einem Fehler den Grund; die Nachricht gilt als nicht gesendet", () => {
    let s = addPending(emptyTranscript(), "p1", "weiter")
    s = applyResumeStep(s, step("acquire", "running"))
    s = applyResumeStep(s, step("acquire", "error", { detail: "Kein freier Platz im Pool" }))
    s = applyResumeStep(s, step("failed", "error", { detail: "Kein freier Platz im Pool", ms: 45000 }))
    s = failPending(s, "p1")
    const r = s.resume!
    expect(r.state).toBe("failed")
    expect(resumeSummary(r)).toBe("Fortsetzen gescheitert: Kein freier Platz im Pool")
    expect(r.steps[0].status).toBe("error")
    expect(s.pending[0].failed).toBe(true)
    // Erneut senden räumt Block und gescheiterte Nachricht weg
    s = addPending(s, "p2", "weiter")
    expect(s.resume).toBeUndefined()
    expect(s.pending.map((p) => p.key)).toEqual(["p2"])
  })

  it("ein neues Fortsetzen (andere Kennung) ersetzt ein altes, fertiges", () => {
    let s = fullResume(emptyTranscript())
    s = applyResumeStep(s, { ...step("acquire", "running"), id: "r2" })
    expect(s.resume!.id).toBe("r2")
    expect(kinds(s)).toEqual(["resume"])
  })
})

describe("Warteschlange", () => {
  it("zeigt übergebene Nachrichten sofort als gesendet und ersetzt sie ohne Doppelung", () => {
    let s = applyQueueDelivered(emptyTranscript(), "q1", "zwei\n\nvier")
    expect(s.pending.map((p) => p.text)).toEqual(["zwei\n\nvier"])
    s = applyPiEvent(s, userEnd("zwei\n\nvier"))
    expect(s.pending).toEqual([])
    expect(kinds(s)).toEqual(["user"])
  })

  it("erweitert eine eben gesendete Nachricht, die zusammen mit Zurückgehaltenem übergeben wird", () => {
    let s = addPending(emptyTranscript(), "p1", "drei\n\n[Anhänge unter /workspace/inputs/]\n- b.csv")
    s = applyQueueDelivered(s, "q1", "zwei\n\ndrei\n\n[Anhänge unter /workspace/inputs/]\n- a.csv\n- b.csv")
    expect(s.pending).toHaveLength(1)
    expect(s.pending[0].key).toBe("p1")
    expect(s.pending[0].text.startsWith("zwei\n\ndrei")).toBe(true)
  })

  it("führt Server-Einträge und noch unbestätigte zusammen", () => {
    const rows = queueRows(
      [{ id: "a", chat_id: "c", text: "eins", attachments: ["x.csv"], created_at: "" }],
      [{ key: "l1", text: "zwei", attachments: [] }],
    )
    expect(rows.map((r) => [r.key, r.sending])).toEqual([
      ["a", false],
      ["l1", true],
    ])
  })

  it("kennzeichnet Systemeinträge (kind system) samt Kurzzeile", () => {
    const note = "Hintergrundaufgabe bg-3 beendet: Exit 0, Laufzeit 0:08\nBefehl: sleep 8\nKeine Ausgabe."
    const rows = queueRows(
      [
        { id: "s", chat_id: "c", text: note, attachments: [], created_at: "", kind: "system", note: "background", refs: ["bg-3"] },
        { id: "u", chat_id: "c", text: note, attachments: [], created_at: "", kind: "user" },
      ],
      [],
    )
    expect(rows.map((r) => [r.system, r.label])).toEqual([
      [true, "Hintergrundaufgabe bg-3 beendet · Exit 0 · 0:08"],
      [false, undefined],
    ])
  })

  it("erwartet Einreihen, solange pi arbeitet, fortgesetzt wird oder eine Sendung unterwegs ist", () => {
    expect(expectQueued({ running: false, resuming: false }, 0)).toBe(false)
    expect(expectQueued({ running: true }, 0)).toBe(true)
    expect(expectQueued({ running: false, resuming: true }, 0)).toBe(true)
    expect(expectQueued({ running: false }, 1)).toBe(true)
  })

  it("kürzt die Vorschau auf eine Zeile", () => {
    expect(queuePreview("a\n\nb   c")).toBe("a b c")
    expect(queuePreview("x".repeat(10), 4)).toBe("xxxx …")
  })
})

// Review 3, H2: Grund des Zurückhaltens und Hinweis bei auto_held.
describe("Zurückgehalten", () => {
  it("nennt den Grund", () => {
    expect(holdReasonText("abort")).toBe("angehalten nach Abbruch")
    expect(holdReasonText("auto_turns")).toContain("ohne Nutzer")
    expect(holdReasonText(undefined)).toBeUndefined()
    expect(autoHeldText({ reason: "auto_turns", limit: 5, count: 5 })).toContain("5 Durchgänge ohne Nutzer in Folge (Grenze 5)")
    expect(autoHeldText({ reason: "wake_limit", limit: 10, count: 10 })).toContain("Weckrufe je Stunde")
  })
})
