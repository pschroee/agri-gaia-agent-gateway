// Sent messages (optimistic), resuming in a fresh sandbox and handing over the
// queue in the history (lib/stream), plus the display (lib/resume, lib/queue).
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

/** Complete resume: all steps done, then ready. */
const fullResume = (s: TranscriptState): TranscriptState =>
  [
    step("acquire", "running"),
    step("acquire", "done", { ms: 120, detail: "p-1" }),
    step("session", "running"),
    step("session", "done", { ms: 300, size: 2048 }),
    step("settings", "running"),
    step("settings", "done", { detail: "internet off" }),
    step("workspace", "running"),
    step("workspace", "done", { size: 1258291, files: 14 }),
    step("inputs", "running"),
    step("inputs", "done", { size: 0, files: 0 }),
    step("ready", "done", { ms: 3400 }),
  ].reduce(applyResumeStep, s)

describe("sent message (optimistic)", () => {
  it("appears right away and is replaced by pi's user message without duplication", () => {
    let s = addPending(emptyTranscript(), "p1", "Hello")
    expect(s.pending.map((p) => p.text)).toEqual(["Hello"])
    s = applyPiEvent(s, userEnd("Hello"))
    expect(s.pending).toEqual([])
    expect(kinds(s)).toEqual(["user"])
  })

  it("replaces the oldest when pi changes the text (skill, template)", () => {
    let s = addPending(emptyTranscript(), "p1", "/skill:report")
    s = addPending(s, "p2", "afterwards")
    s = applyPiEvent(s, userEnd("<skill>…</skill>"))
    expect(s.pending.map((p) => p.key)).toEqual(["p2"])
  })

  it("keeps attachments in the text (the same form the server appends)", () => {
    const text = "Look\n\n[Attachments in /workspace/inputs/]\n- a.csv"
    const s = applyPiEvent(addPending(emptyTranscript(), "p1", text), userEnd(text))
    expect(s.pending).toEqual([])
    expect(s.items[0].kind === "user" && s.items[0].text).toBe(text)
  })

  it("stays as not sent on an error and disappears with the next send", () => {
    let s = failPending(addPending(emptyTranscript(), "p1", "one"), "p1")
    expect(s.pending[0].failed).toBe(true)
    // pi's user message does not replace a failed one
    s = addPending(s, "p2", "one")
    expect(s.pending.map((p) => p.key)).toEqual(["p2"])
  })

  it("can be removed", () => {
    expect(dropPending(addPending(emptyTranscript(), "p1", "x"), "p1").pending).toEqual([])
  })

  it("survives a reload until the history contains it", () => {
    let s = hydrate(emptyTranscript(), [stored(1, "user", "old"), stored(2, "assistant", "ok")])
    s = addPending(s, "p1", "new")
    expect(s.pending[0].afterSeq).toBe(2)
    // history still without the new message: stays
    s = hydrate(s, [stored(1, "user", "old"), stored(2, "assistant", "ok")])
    expect(s.pending).toHaveLength(1)
    // the same text before the anchor does not count
    s = hydrate(s, [stored(1, "user", "new"), stored(2, "assistant", "ok")])
    expect(s.pending).toHaveLength(1)
    s = hydrate(s, [stored(1, "user", "old"), stored(2, "assistant", "ok"), stored(3, "user", "new")])
    expect(s.pending).toEqual([])
    expect(kinds(s)).toEqual(["user", "assistant", "user"])
  })
})

describe("resuming in a fresh sandbox", () => {
  it("shows all steps from the start, the running one as running", () => {
    let s = addPending(emptyTranscript(), "p1", "continue")
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
    expect(resumeSummary(r)).toBe("Resuming chat …")
    // no "Thinking …" while resuming
    expect(awaitingAnswer(s)).toBe(false)
  })

  it("takes duration, size and file count and collapses to one line after ready", () => {
    const s = fullResume(addPending(emptyTranscript(), "p1", "continue"))
    const r = s.resume!
    expect(r.state).toBe("done")
    expect(resumeSummary(r)).toBe("Resumed in a fresh sandbox · 3.4 s")
    const ws = r.steps.find((x) => x.phase === "workspace")!
    expect(stepDetail(ws)).toBe("1.2 MiB, 14 files")
    expect(stepDetail(r.steps.find((x) => x.phase === "inputs")!)).toBe("no files")
    expect(stepDetail(r.steps.find((x) => x.phase === "acquire")!)).toBeUndefined()
    expect(stepDetail(r.steps.find((x) => x.phase === "session")!)).toBe("2.0 KiB")
    expect(awaitingAnswer(s)).toBe(true)
  })

  it("stands after the user message in the history, followed by the response", () => {
    let s = fullResume(addPending(emptyTranscript(), "p1", "continue"))
    s = applyPiEvent(s, { type: "agent_start" })
    s = applyPiEvent(s, userEnd("continue"))
    expect(s.resume).toBeUndefined()
    expect(s.pending).toEqual([])
    expect(kinds(s)).toEqual(["user", "resume"])
    s = applyPiEvent(s, assistantStart)
    expect(kinds(s)).toEqual(["user", "resume", "assistant"])
  })

  it("moves before the next entry if no user message follows (/compact)", () => {
    let s = fullResume(emptyTranscript())
    s = applyPiEvent(s, { type: "compaction_start", reason: "manual" })
    expect(kinds(s)).toEqual(["resume", "compaction"])
  })

  it("stays behind the user message on reload", () => {
    let s = hydrate(emptyTranscript(), [stored(1, "user", "a"), stored(2, "assistant", "b")])
    s = fullResume(addPending(s, "p1", "continue"))
    s = applyPiEvent(s, userEnd("continue"))
    s = applyPiEvent(s, assistantStart)
    s = applyPiEvent(s, { type: "message_end", message: { role: "assistant", content: [{ type: "text", text: "c" }] } })
    s = hydrate(s, [stored(1, "user", "a"), stored(2, "assistant", "b"), stored(3, "user", "continue"), stored(4, "assistant", "c")])
    expect(kinds(s)).toEqual(["user", "assistant", "user", "resume", "assistant"])
    const r = s.items[3] as ResumeItem
    expect(r.state).toBe("done")
  })

  it("shows the reason on an error; the message counts as not sent", () => {
    let s = addPending(emptyTranscript(), "p1", "continue")
    s = applyResumeStep(s, step("acquire", "running"))
    s = applyResumeStep(s, step("acquire", "error", { detail: "no free slot in the pool" }))
    s = applyResumeStep(s, step("failed", "error", { detail: "no free slot in the pool", ms: 45000 }))
    s = failPending(s, "p1")
    const r = s.resume!
    expect(r.state).toBe("failed")
    expect(resumeSummary(r)).toBe("Resuming failed: no free slot in the pool")
    expect(r.steps[0].status).toBe("error")
    expect(s.pending[0].failed).toBe(true)
    // sending again clears the block and the failed message
    s = addPending(s, "p2", "continue")
    expect(s.resume).toBeUndefined()
    expect(s.pending.map((p) => p.key)).toEqual(["p2"])
  })

  it("a new resume (different ID) replaces an old finished one", () => {
    let s = fullResume(emptyTranscript())
    s = applyResumeStep(s, { ...step("acquire", "running"), id: "r2" })
    expect(s.resume!.id).toBe("r2")
    expect(kinds(s)).toEqual(["resume"])
  })
})

describe("queue", () => {
  it("shows handed-over messages right away as sent and replaces them without duplication", () => {
    let s = applyQueueDelivered(emptyTranscript(), "q1", "two\n\nfour")
    expect(s.pending.map((p) => p.text)).toEqual(["two\n\nfour"])
    s = applyPiEvent(s, userEnd("two\n\nfour"))
    expect(s.pending).toEqual([])
    expect(kinds(s)).toEqual(["user"])
  })

  it("extends a just-sent message that is handed over together with held-back ones", () => {
    let s = addPending(emptyTranscript(), "p1", "three\n\n[Attachments in /workspace/inputs/]\n- b.csv")
    s = applyQueueDelivered(s, "q1", "two\n\nthree\n\n[Attachments in /workspace/inputs/]\n- a.csv\n- b.csv")
    expect(s.pending).toHaveLength(1)
    expect(s.pending[0].key).toBe("p1")
    expect(s.pending[0].text.startsWith("two\n\nthree")).toBe(true)
  })

  it("merges server entries and not yet confirmed ones", () => {
    const rows = queueRows(
      [{ id: "a", chat_id: "c", text: "one", attachments: ["x.csv"], created_at: "" }],
      [{ key: "l1", text: "two", attachments: [] }],
    )
    expect(rows.map((r) => [r.key, r.sending])).toEqual([
      ["a", false],
      ["l1", true],
    ])
  })

  it("marks system entries (kind system) with their short line", () => {
    // the note text comes from internal/chat/background.go and is still German there
    const note = "Background task bg-3 finished: exit 0, runtime 0:08\nCommand: sleep 8\nNo output."
    const rows = queueRows(
      [
        { id: "s", chat_id: "c", text: note, attachments: [], created_at: "", kind: "system", note: "background", refs: ["bg-3"] },
        { id: "u", chat_id: "c", text: note, attachments: [], created_at: "", kind: "user" },
      ],
      [],
    )
    expect(rows.map((r) => [r.system, r.label])).toEqual([
      [true, "Background task bg-3 finished · exit 0 · 0:08"],
      [false, undefined],
    ])
  })

  it("expects enqueueing while pi is working, resuming or a send is in flight", () => {
    expect(expectQueued({ running: false, resuming: false }, 0)).toBe(false)
    expect(expectQueued({ running: true }, 0)).toBe(true)
    expect(expectQueued({ running: false, resuming: true }, 0)).toBe(true)
    expect(expectQueued({ running: false }, 1)).toBe(true)
  })

  it("shortens the preview to one line", () => {
    expect(queuePreview("a\n\nb   c")).toBe("a b c")
    expect(queuePreview("x".repeat(10), 4)).toBe("xxxx …")
  })
})

// Review 3, H2: reason for holding back and notice on auto_held.
describe("held back", () => {
  it("names the reason", () => {
    expect(holdReasonText("abort")).toBe("paused after abort")
    expect(holdReasonText("auto_turns")).toContain("without the user")
    expect(holdReasonText(undefined)).toBeUndefined()
    expect(autoHeldText({ reason: "auto_turns", limit: 5, count: 5 })).toContain("5 turns without the user in a row (limit 5)")
    expect(autoHeldText({ reason: "wake_limit", limit: 10, count: 10 })).toContain("wake-ups per hour")
  })
})
