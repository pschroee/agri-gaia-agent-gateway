import { describe, expect, it } from "vitest"
import { describeToolArgs, firstLine } from "./toolargs"

describe("firstLine", () => {
  it("leaves single-line texts unchanged", () => {
    expect(firstLine("ls -la")).toBe("ls -la")
  })
  it("shortens multi-line texts to the first line with an ellipsis", () => {
    expect(firstLine("cd /workspace\npython3 train.py")).toBe("cd /workspace …")
  })
  it("skips leading blank lines", () => {
    expect(firstLine("\n\n  echo hi\nexit")).toBe("echo hi …")
  })
  it("returns an empty string for empty text", () => {
    expect(firstLine("")).toBe("")
    expect(firstLine("\n \n")).toBe("")
  })
})

describe("describeToolArgs", () => {
  it("shows the bash command as a code block with real line breaks", () => {
    const d = describeToolArgs("bash", { command: "cat <<EOF > a.txt\nhello\nEOF", timeout: 30 })
    expect(d.summary).toBe("cat <<EOF > a.txt …")
    expect(d.sections[0]).toEqual({ label: "Command", kind: "code", text: "cat <<EOF > a.txt\nhello\nEOF" })
    expect(d.sections[1]).toEqual({ label: "Further arguments", kind: "json", text: '{\n  "timeout": 30\n}' })
  })

  it("highlights the path for write and shows the content as a code block", () => {
    const d = describeToolArgs("write", { path: "/workspace/a.py", content: "print(1)\nprint(2)\n" })
    expect(d.summary).toBe("/workspace/a.py")
    expect(d.sections).toEqual([
      { label: "Path", kind: "path", text: "/workspace/a.py" },
      { label: "Content", kind: "code", text: "print(1)\nprint(2)\n" },
    ])
  })

  it("shows each edit replacement as a pair of old and new text", () => {
    const d = describeToolArgs("edit", {
      path: "a.py",
      edits: [
        { oldText: "x = 1", newText: "x = 2" },
        { oldText: "y", newText: "z" },
      ],
    })
    expect(d.summary).toBe("a.py")
    expect(d.sections).toEqual([
      { label: "Path", kind: "path", text: "a.py" },
      { label: "Replacement 1: old", kind: "code", text: "x = 1" },
      { label: "Replacement 1: new", kind: "code", text: "x = 2" },
      { label: "Replacement 2: old", kind: "code", text: "y" },
      { label: "Replacement 2: new", kind: "code", text: "z" },
    ])
  })

  it("also understands the old edit form with oldText/newText at the top level", () => {
    const d = describeToolArgs("edit", { path: "a.py", oldText: "a", newText: "b" })
    expect(d.sections.map((s) => s.label)).toEqual(["Path", "Replacement: old", "Replacement: new"])
  })

  it("formats unknown tools as indented JSON", () => {
    const d = describeToolArgs("agw_upload", { name: "report.pdf", note: "a\nb" })
    expect(d.summary).toBe("report.pdf")
    expect(d.sections).toEqual([{ label: "Arguments", kind: "json", text: '{\n  "name": "report.pdf",\n  "note": "a\\nb"\n}' }])
  })

  it("shortens the summary line to the first line for unknown tools too", () => {
    expect(describeToolArgs("search", { query: "a\nb" }).summary).toBe("a …")
  })

  it("shows raw JSON still being streamed unchanged", () => {
    const d = describeToolArgs("bash", undefined, '{"command": "ls')
    expect(d.summary).toBe("")
    expect(d.sections).toEqual([{ label: "Arguments", kind: "json", text: '{"command": "ls' }])
  })

  it("falls back to JSON for bash without command", () => {
    const d = describeToolArgs("bash", { foo: 1 })
    expect(d.sections).toEqual([{ label: "Arguments", kind: "json", text: '{\n  "foo": 1\n}' }])
  })

  it("returns an empty section without arguments", () => {
    expect(describeToolArgs("x", undefined).sections).toEqual([{ label: "Arguments", kind: "json", text: "" }])
  })

  it("does not show arguments with null or undefined as \"Further arguments\"", () => {
    const d = describeToolArgs("bash", { command: "ls", timeout: null, cwd: undefined })
    expect(d.sections).toEqual([{ label: "Command", kind: "code", text: "ls" }])
    const w = describeToolArgs("write", { path: "/a", content: "x", mode: null, enc: "utf8" })
    expect(w.sections[2]).toEqual({ label: "Further arguments", kind: "json", text: '{\n  "enc": "utf8"\n}' })
  })

  it("leaves out null values in the generic argument block too", () => {
    const d = describeToolArgs("search", { query: "a", limit: null })
    expect(d.sections).toEqual([{ label: "Arguments", kind: "json", text: '{\n  "query": "a"\n}' }])
  })
})
