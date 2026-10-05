// Hintergrundaufgaben (bash mit run_in_background): Stand aus der API und den SSE-Ereignissen
// „background“, Beschriftungen und Laufzeiten für Seitenreiter und Chatkopf.
import type { BackgroundEvent, BackgroundState, BackgroundTask } from "@/api/types"
import { formatElapsed } from "@/lib/runtime"

/** Übernimmt ein Ereignis. Ein gedrosseltes „output“ überschreibt ein schon beendetes Ende nicht. */
export function applyBackgroundEvent(list: BackgroundTask[], ev: BackgroundEvent): BackgroundTask[] {
  const t = ev.task
  if (!t?.id) return list
  const idx = list.findIndex((x) => x.id === t.id)
  if (idx < 0) return [...list, t]
  if (ev.change === "output" && list[idx].state !== "running") return list
  const next = list.slice()
  next[idx] = { ...list[idx], ...t }
  return next
}

/** Laufende zuerst (älteste oben), danach beendete (neueste oben). */
export function sortBackground(list: BackgroundTask[]): BackgroundTask[] {
  return [...list].sort((a, b) => {
    const ra = a.state === "running" ? 0 : 1
    const rb = b.state === "running" ? 0 : 1
    if (ra !== rb) return ra - rb
    return ra === 0 ? a.seq - b.seq : b.seq - a.seq
  })
}

export const runningCount = (list: BackgroundTask[]) => list.filter((t) => t.state === "running").length

export type Tone = "running" | "ok" | "error" | "muted"

/** Deutsche Beschriftung und Farbton eines Zustands. */
export function backgroundStatus(t: Pick<BackgroundTask, "state" | "exit_code" | "stopped_by" | "error">): { label: string; tone: Tone } {
  const labels: Record<BackgroundState, string> = {
    running: "läuft",
    exited: "beendet",
    failed: "fehlgeschlagen",
    timeout: "Zeitgrenze",
    stopped: t.stopped_by === "user" ? "vom Nutzer gestoppt" : "vom Agenten gestoppt",
    lost: "mit der Sandbox verloren",
    suspended: "beim Ruhen beendet",
    closed: "mit dem Chat beendet",
  }
  switch (t.state) {
    case "running":
      return { label: labels.running, tone: "running" }
    case "exited":
      return t.exit_code === 0
        ? { label: "beendet (Exit 0)", tone: "ok" }
        : { label: `beendet (Exit ${t.exit_code ?? "?"})`, tone: "error" }
    case "failed":
    case "timeout":
      return { label: labels[t.state], tone: "error" }
    default:
      return { label: labels[t.state] ?? t.state, tone: "muted" }
  }
}

/** Laufzeit: bis `now`, solange die Aufgabe läuft; sonst bis zum Ende; ohne Zeiten nichts. */
export function backgroundRuntimeMs(t: Pick<BackgroundTask, "state" | "started_at" | "ended_at">, now: number): number | undefined {
  const start = Date.parse(t.started_at)
  if (Number.isNaN(start)) return undefined
  if (t.state === "running") return Math.max(0, now - start)
  const end = t.ended_at ? Date.parse(t.ended_at) : NaN
  return Number.isNaN(end) ? undefined : Math.max(0, end - start)
}

export function formatBackgroundRuntime(t: Pick<BackgroundTask, "state" | "started_at" | "ended_at">, now: number): string {
  const ms = backgroundRuntimeMs(t, now)
  return ms === undefined ? "" : formatElapsed(ms)
}

/** Die letzten n Zeilen der Ausgabe (ohne abschließenden Zeilenumbruch). */
export function tailLines(tail: string | undefined, n: number): string[] {
  const s = (tail ?? "").replace(/\n+$/, "")
  if (!s) return []
  const lines = s.split("\n")
  return lines.slice(Math.max(0, lines.length - n))
}

/** Kurzform des Befehls für eine Zeile. */
export function commandPreview(cmd: string, max = 120): string {
  const one = cmd.split(/\s+/).filter(Boolean).join(" ")
  return one.length > max ? `${one.slice(0, max).trimEnd()} …` : one
}

/** Beschriftung des Zählers im Chatkopf. */
export function backgroundCountLabel(n: number): string {
  return n === 1 ? "1 Hintergrundaufgabe läuft" : `${n} Hintergrundaufgaben laufen`
}

/** Werkzeugaufruf, der eine Hintergrundaufgabe startet, abfragt oder stoppt. */
export type BackgroundCall = { kind: "start" | "output" | "stop"; id?: string; task?: BackgroundTask }

const isObj = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v)

/**
 * Erkennt bash mit run_in_background (Aufgabe über tool_call_id, sonst Kennung aus dem Ergebnis
 * „Background task bg-3 started …“) sowie bg_output und bg_stop; sonst undefined.
 */
export function backgroundCall(
  name: string,
  args: unknown,
  toolCallId: string | undefined,
  result: string | undefined,
  tasks: BackgroundTask[],
): BackgroundCall | undefined {
  const a = isObj(args) ? args : {}
  if (name === "bash") {
    const task = toolCallId ? tasks.find((t) => t.tool_call_id === toolCallId) : undefined
    if (!task && a.run_in_background !== true) return undefined
    const id = task?.id ?? /\bBackground task (bg-\d+) started\b/.exec(result ?? "")?.[1]
    const out: BackgroundCall = { kind: "start" }
    if (id) out.id = id
    if (task) out.task = task
    return out
  }
  if (name === "bg_output" || name === "bg_stop") {
    const out: BackgroundCall = { kind: name === "bg_output" ? "output" : "stop" }
    if (typeof a.id === "string" && a.id) {
      out.id = a.id
      const task = tasks.find((t) => t.id === a.id)
      if (task) out.task = task
    }
    return out
  }
  return undefined
}

/** Kompakte Zeile für bg_output und bg_stop. */
export function backgroundCallLabel(c: Pick<BackgroundCall, "kind" | "id">, phase: "running" | "done" | "error"): string {
  const id = c.id ?? "Hintergrundaufgabe"
  if (c.kind === "output") {
    return phase === "running" ? `Ausgabe von ${id} wird abgerufen …` : phase === "error" ? `Ausgabe von ${id} nicht abgerufen` : `Ausgabe von ${id} abgerufen`
  }
  return phase === "running" ? `${id} wird gestoppt …` : phase === "error" ? `${id} nicht gestoppt` : `${id} gestoppt`
}
