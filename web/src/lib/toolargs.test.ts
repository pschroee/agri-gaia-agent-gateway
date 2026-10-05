import { describe, expect, it } from "vitest"
import { describeToolArgs, firstLine } from "./toolargs"

describe("firstLine", () => {
  it("lässt einzeilige Texte unverändert", () => {
    expect(firstLine("ls -la")).toBe("ls -la")
  })
  it("kürzt mehrzeilige Texte auf die erste Zeile mit Auslassungszeichen", () => {
    expect(firstLine("cd /workspace\npython3 train.py")).toBe("cd /workspace …")
  })
  it("überspringt führende Leerzeilen", () => {
    expect(firstLine("\n\n  echo hi\nexit")).toBe("echo hi …")
  })
  it("liefert bei leerem Text einen leeren String", () => {
    expect(firstLine("")).toBe("")
    expect(firstLine("\n \n")).toBe("")
  })
})

describe("describeToolArgs", () => {
  it("zeigt bei bash den Befehl als Codeblock mit echten Zeilenumbrüchen", () => {
    const d = describeToolArgs("bash", { command: "cat <<EOF > a.txt\nhallo\nEOF", timeout: 30 })
    expect(d.summary).toBe("cat <<EOF > a.txt …")
    expect(d.sections[0]).toEqual({ label: "Befehl", kind: "code", text: "cat <<EOF > a.txt\nhallo\nEOF" })
    expect(d.sections[1]).toEqual({ label: "Weitere Argumente", kind: "json", text: '{\n  "timeout": 30\n}' })
  })

  it("hebt bei write den Pfad hervor und zeigt den Inhalt als Codeblock", () => {
    const d = describeToolArgs("write", { path: "/workspace/a.py", content: "print(1)\nprint(2)\n" })
    expect(d.summary).toBe("/workspace/a.py")
    expect(d.sections).toEqual([
      { label: "Pfad", kind: "path", text: "/workspace/a.py" },
      { label: "Inhalt", kind: "code", text: "print(1)\nprint(2)\n" },
    ])
  })

  it("zeigt bei edit jede Ersetzung als Paar aus altem und neuem Text", () => {
    const d = describeToolArgs("edit", {
      path: "a.py",
      edits: [
        { oldText: "x = 1", newText: "x = 2" },
        { oldText: "y", newText: "z" },
      ],
    })
    expect(d.summary).toBe("a.py")
    expect(d.sections).toEqual([
      { label: "Pfad", kind: "path", text: "a.py" },
      { label: "Ersetzung 1: alt", kind: "code", text: "x = 1" },
      { label: "Ersetzung 1: neu", kind: "code", text: "x = 2" },
      { label: "Ersetzung 2: alt", kind: "code", text: "y" },
      { label: "Ersetzung 2: neu", kind: "code", text: "z" },
    ])
  })

  it("versteht bei edit auch die Altform mit oldText/newText auf oberster Ebene", () => {
    const d = describeToolArgs("edit", { path: "a.py", oldText: "a", newText: "b" })
    expect(d.sections.map((s) => s.label)).toEqual(["Pfad", "Ersetzung: alt", "Ersetzung: neu"])
  })

  it("formatiert unbekannte Werkzeuge als eingerücktes JSON", () => {
    const d = describeToolArgs("agw_upload", { name: "bericht.pdf", note: "a\nb" })
    expect(d.summary).toBe("bericht.pdf")
    expect(d.sections).toEqual([{ label: "Argumente", kind: "json", text: '{\n  "name": "bericht.pdf",\n  "note": "a\\nb"\n}' }])
  })

  it("kürzt auch bei unbekannten Werkzeugen die Kurzzeile auf die erste Zeile", () => {
    expect(describeToolArgs("suche", { query: "a\nb" }).summary).toBe("a …")
  })

  it("zeigt noch gestreamtes Roh-JSON unverändert", () => {
    const d = describeToolArgs("bash", undefined, '{"command": "ls')
    expect(d.summary).toBe("")
    expect(d.sections).toEqual([{ label: "Argumente", kind: "json", text: '{"command": "ls' }])
  })

  it("fällt bei bash ohne command auf JSON zurück", () => {
    const d = describeToolArgs("bash", { foo: 1 })
    expect(d.sections).toEqual([{ label: "Argumente", kind: "json", text: '{\n  "foo": 1\n}' }])
  })

  it("liefert ohne Argumente einen leeren Abschnitt", () => {
    expect(describeToolArgs("x", undefined).sections).toEqual([{ label: "Argumente", kind: "json", text: "" }])
  })

  it("zeigt Argumente mit null oder undefined nicht als „Weitere Argumente“", () => {
    const d = describeToolArgs("bash", { command: "ls", timeout: null, cwd: undefined })
    expect(d.sections).toEqual([{ label: "Befehl", kind: "code", text: "ls" }])
    const w = describeToolArgs("write", { path: "/a", content: "x", mode: null, enc: "utf8" })
    expect(w.sections[2]).toEqual({ label: "Weitere Argumente", kind: "json", text: '{\n  "enc": "utf8"\n}' })
  })

  it("lässt null-Werte auch im allgemeinen Argumentblock weg", () => {
    const d = describeToolArgs("suche", { query: "a", limit: null })
    expect(d.sections).toEqual([{ label: "Argumente", kind: "json", text: '{\n  "query": "a"\n}' }])
  })
})
