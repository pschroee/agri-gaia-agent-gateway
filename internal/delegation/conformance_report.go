// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

// Report of the conformance check without a language model (outline 7.3.1, X10): per case the
// identifier, the call, the delegation, the expected and the actual decision, the reason and the
// check path, plus the gateway version and the time. Two test packages write it — the pure check in
// this package and the one through the manager in internal/chat — so each writes its own section as
// a partial JSON file; the last writer composes both into the Markdown and JSON report.

// Check paths of the report.
const (
	CheckPure    = "pure"    // authorization service alone, without the platform
	CheckManager = "manager" // through the manager and the platform
)

// Meta identifies the gateway build and the time of one check run.
type Meta struct {
	Version string    `json:"version"`
	Time    time.Time `json:"time"`
}

// Result is one checked case in one check path.
type Result struct {
	Case       string `json:"case"`       // identifier and description (Case.Name)
	Group      string `json:"group"`      // class of the case (control, parser, ...)
	Call       string `json:"call"`       // tool with arguments or method and path
	Delegation string `json:"delegation"` // the delegation the case runs with
	Want       bool   `json:"want"`       // expected decision
	Got        bool   `json:"got"`        // actual decision
	Why        string `json:"why"`        // reason for the actual decision
	Path       string `json:"path"`       // CheckPure or CheckManager
}

// ResultFor turns one checked case into a report row.
func ResultFor(c Case, path string, got bool, why string) Result {
	return Result{
		Case:       c.Name,
		Group:      c.Group,
		Call:       c.CallText(),
		Delegation: c.DelegationText(),
		Want:       c.Allowed,
		Got:        got,
		Why:        why,
		Path:       path,
	}
}

// Report summarizes one check path.
type Report struct {
	Meta       Meta     `json:"meta"`
	Check      string   `json:"check"`
	Cases      int      `json:"cases"`
	Forbidden  int      `json:"forbidden"` // expected refusals
	Controls   int      `json:"controls"`  // allowed control calls
	Refused    int      `json:"refused"`   // actual refusals
	Deviations []Result `json:"deviations"`
	Results    []Result `json:"results"`
}

// NewReport counts the rows of one check path. The path is taken from the rows; without rows the
// report is empty and still carries the meta data.
func NewReport(results []Result, meta Meta) Report {
	r := Report{Meta: meta, Results: results}
	for _, x := range results {
		if r.Check == "" {
			r.Check = x.Path
		}
		r.Cases++
		if x.Want {
			r.Controls++
		} else {
			r.Forbidden++
		}
		if !x.Got {
			r.Refused++
		}
		if x.Want != x.Got {
			r.Deviations = append(r.Deviations, x)
		}
	}
	return r
}

// CheckTitle names a check path in the report.
func CheckTitle(check string) string {
	switch check {
	case CheckPure:
		return "pure (without the manager)"
	case CheckManager:
		return "through the manager and the platform"
	}
	return check
}

// Markdown renders one check path as a table with a summary. Every deviation follows the table.
func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Check: %s\n\n", CheckTitle(r.Check))
	fmt.Fprintf(&b, "Cases: %d (expected refusals: %d, allowed controls: %d)\n\n", r.Cases, r.Forbidden, r.Controls)
	fmt.Fprintf(&b, "Refused: %d · deviations: %d\n\n", r.Refused, len(r.Deviations))
	b.WriteString("| Case | Group | Call | Delegation | Expected | Actual | Reason |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, x := range r.Results {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
			mdCell(x.Case), mdCell(x.Group), mdCell(x.Call), mdCell(x.Delegation),
			decision(x.Want), decision(x.Got), mdCell(x.Why))
	}
	if len(r.Deviations) > 0 {
		b.WriteString("\nDeviations:\n\n")
		for _, x := range r.Deviations {
			fmt.Fprintf(&b, "- %s [%s]: expected %s, got %s (%s)\n", x.Case, x.Group, decision(x.Want), decision(x.Got), x.Why)
		}
	}
	return b.String()
}

// ReportMarkdown renders the results of one check path as Markdown (X10.1).
func ReportMarkdown(results []Result, meta Meta) string {
	r := NewReport(results, meta)
	var b strings.Builder
	b.WriteString(reportHeader(meta))
	b.WriteString(r.Markdown())
	return b.String()
}

// ReportJSON renders the results of one check path as JSON (X10.1).
func ReportJSON(results []Result, meta Meta) ([]byte, error) {
	return json.MarshalIndent(NewReport(results, meta), "", "  ")
}

// reportDocument is the combined report of all check paths.
type reportDocument struct {
	Version string   `json:"version"`
	Time    string   `json:"time"`
	Checks  []Report `json:"checks"`
}

// WriteConformanceReport writes the report of one check path when path is not empty: the partial
// file <base>.<check>.json, then the composed report as <path> (Markdown) and <path>.json. Another
// test package may write its own check at the same time; partial files are replaced atomically and
// the last writer composes every partial it finds, so the composed report names both paths.
func WriteConformanceReport(path string, meta Meta, results []Result) error {
	if path == "" {
		return nil
	}
	check := CheckPure
	if len(results) > 0 && results[0].Path != "" {
		check = results[0].Path
	}
	mdPath, jsPath := path, jsonPath(path)
	base := strings.TrimSuffix(mdPath, filepath.Ext(mdPath))
	dir := filepath.Dir(mdPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	partial, err := ReportJSON(results, meta)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(base+"."+check+".json", partial); err != nil {
		return err
	}
	doc := reportDocument{Version: meta.Version, Time: meta.Time.Format(time.RFC3339)}
	for _, c := range []string{CheckPure, CheckManager} {
		data, err := os.ReadFile(base + "." + c + ".json")
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		var r Report
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("conformance report %s.%s.json: %w", base, c, err)
		}
		doc.Checks = append(doc.Checks, r)
	}
	var b strings.Builder
	b.WriteString("# Conformance check without a language model (outline 7.3.1)\n\n")
	b.WriteString(reportHeader(meta))
	for i := range doc.Checks {
		b.WriteString(doc.Checks[i].Markdown())
		b.WriteString("\n")
	}
	if err := writeFileAtomic(mdPath, []byte(b.String())); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(jsPath, data)
}

// reportHeader names the build and the time of the run.
func reportHeader(meta Meta) string {
	return fmt.Sprintf("Gateway version: %s\nTime: %s\n\n", meta.Version, meta.Time.Format(time.RFC3339))
}

// jsonPath derives the JSON report from the Markdown path: conformance.md → conformance.json.
func jsonPath(md string) string {
	if strings.HasSuffix(md, ".md") {
		return strings.TrimSuffix(md, ".md") + ".json"
	}
	return md + ".json"
}

// BuildVersion names the gateway build the report came from: the module version if the build
// carries one, otherwise the VCS revision, otherwise "unknown". The tests run from a source tree,
// so they usually report the revision (with -dirty when the tree has changes).
func BuildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var rev string
	modified := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		if modified {
			rev += "-dirty"
		}
		return rev
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "unknown"
}

// writeFileAtomic writes data through a temporary file in the same directory and renames it, so
// that a reader never sees a half-written report.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".conformance-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// decision renders a decision for the report.
func decision(allowed bool) string {
	if allowed {
		return "allowed"
	}
	return "refused"
}

// mdCell keeps a value inside its table cell.
func mdCell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ")
}
