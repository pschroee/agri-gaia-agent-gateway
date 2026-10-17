// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agw/internal/agwclient"
	"agw/internal/exp"
)

// cmdExp dispatches the experiment subcommands: validate (X9), seed, unseed and verify of the fixtures (X4,
// issue #129), run, status and abort of the runner (X1), and export of the protocol (X5).
func (a *app) cmdExp(args []string) error {
	if len(args) == 0 {
		return usagef("exp: subcommand missing (validate, seed, unseed, verify, run, status, abort, export)")
	}
	switch args[0] {
	case "validate":
		return a.cmdExpValidate(args[1:])
	case "seed":
		return a.cmdExpSeed(args[1:])
	case "unseed":
		return a.cmdExpUnseed(args[1:])
	case "verify":
		return a.cmdExpVerify(args[1:])
	case "run":
		return a.cmdExpRun(args[1:])
	case "status":
		return a.cmdExpStatus(args[1:])
	case "abort":
		return a.cmdExpAbort(args[1:])
	case "export":
		return a.cmdExpExport(args[1:])
	}
	return usagef("unknown command \"exp %s\" – agw help shows all commands", args[0])
}

// material is the experiment material of one plan, read from disk. PlanID names the plan file
// without its extension (plan or pilot) and is the key under which the fixtures are recorded.
type material struct {
	Dir       string
	Path      string
	PlanID    string
	Plan      *exp.Plan
	Tasks     []exp.Task
	Manifests []exp.Manifest
}

// loadMaterial reads a plan directory (then plan.yaml is used) or a plan file.
func loadMaterial(path string) (*material, error) {
	m := &material{Path: path}
	var err error
	if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
		m.Dir = path
		m.Path = filepath.Join(path, "plan.yaml")
		m.Plan, m.Tasks, m.Manifests, err = exp.LoadPlan(path)
	} else {
		m.Dir = filepath.Dir(path)
		m.Plan, m.Tasks, m.Manifests, err = exp.LoadPlanFile(path)
	}
	if err != nil {
		return nil, err
	}
	base := filepath.Base(m.Path)
	m.PlanID = strings.TrimSuffix(base, filepath.Ext(base))
	return m, nil
}

// cellOf checks that the cell belongs to the plan. Seeding a cell the plan does not list would
// create objects for a cell that never runs.
func (m *material) cellOf(id string) (exp.Cell, error) {
	for _, c := range m.Plan.Cells {
		if c.ID == id {
			return c, nil
		}
	}
	return exp.Cell{}, fmt.Errorf("cell %q is not listed in %s", id, filepath.Base(m.Path))
}

// expToken switches the client to the experiment key. The runner on the server uses
// AGW_EXP_API_TOKEN; the fixture routes refuse the UI's token (X1.3/X4).
func (a *app) expToken() {
	if tok := os.Getenv("AGW_EXP_API_TOKEN"); tok != "" {
		a.c.Token = tok
	}
}

// fixtureRow is one recorded fixture as the API returns it. FieldSHA256 carries hashes only, never
// contents (X4.7).
type fixtureRow struct {
	ID           string            `json:"id"`
	Plan         string            `json:"plan"`
	Cell         string            `json:"cell"`
	ManifestID   string            `json:"manifest_id"`
	Kind         string            `json:"kind"`
	Owner        string            `json:"owner"`
	ObjectID     string            `json:"object_id"`
	FieldSHA256  map[string]string `json:"field_sha256"`
	CreatedAt    time.Time         `json:"created_at"`
	RemovedAt    *time.Time        `json:"removed_at,omitempty"`
	RemoveResult string            `json:"remove_result,omitempty"`
}

type fixtureAnswer struct {
	Fixtures   []fixtureRow  `json:"fixtures"`
	Removed    int           `json:"removed"`
	Checked    int           `json:"checked"`
	State      string        `json:"state"`
	Error      string        `json:"error"`
	Mismatches []mismatchRow `json:"mismatches"`
}

// mismatchRow is one finding of a fixture_mismatch. It carries hashes, not values (X4.7).
type mismatchRow struct {
	ManifestID string             `json:"manifest_id"`
	Kind       string             `json:"kind"`
	ObjectID   string             `json:"object_id"`
	Missing    bool               `json:"missing"`
	Fields     []fieldMismatchRow `json:"fields"`
}

type fieldMismatchRow struct {
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// cmdExpSeed creates the fixtures of one cell from the material and prints their identifiers and
// checksums. On fixture_mismatch it prints the differing fields (hash only) and exits 1 (X4.4).
func (a *app) cmdExpSeed(args []string) error {
	fs := a.flags("exp seed")
	path := fs.String("plan", "", "material directory with plan.yaml, or a plan file")
	cell := fs.String("cell", "", "cell of the plan")
	if _, err := a.parse(fs, args, 0, 0, "agw exp seed --plan <dir|file> --cell <id>"); err != nil {
		return err
	}
	if *path == "" || *cell == "" {
		return usagef("--plan and --cell are required")
	}
	m, err := loadMaterial(*path)
	if err != nil {
		return err
	}
	if _, err := m.cellOf(*cell); err != nil {
		return err
	}
	specs, err := exp.Specs(m.Dir, exp.ForCell(m.Manifests, *cell))
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		fmt.Fprintf(a.stdout, "no fixtures for cell %s\n", *cell)
		return nil
	}
	a.expToken()
	in := map[string]any{"plan": m.PlanID, "cell": *cell, "fixtures": specs}
	var out fixtureAnswer
	status, err := a.c.DoStatus(a.ctx, "POST", "/api/exp/fixtures/seed", in, &out, 409)
	if err != nil {
		return err
	}
	if a.json {
		if err := a.printJSON(out); err != nil {
			return err
		}
	} else {
		printFixtures(a, out.Fixtures)
	}
	if status == 409 {
		printMismatches(a, out.Mismatches)
		return errSilentFailure
	}
	return nil
}

// cmdExpUnseed removes the live fixtures of a cell by the identifiers recorded at seeding. Objects
// of the same name that this cell did not create stay (CLAUDE.md).
func (a *app) cmdExpUnseed(args []string) error {
	fs := a.flags("exp unseed")
	path := fs.String("plan", "", "material directory with plan.yaml, or a plan file")
	cell := fs.String("cell", "", "cell of the plan")
	if _, err := a.parse(fs, args, 0, 0, "agw exp unseed --plan <dir|file> --cell <id>"); err != nil {
		return err
	}
	if *path == "" || *cell == "" {
		return usagef("--plan and --cell are required")
	}
	m, err := loadMaterial(*path)
	if err != nil {
		return err
	}
	if _, err := m.cellOf(*cell); err != nil {
		return err
	}
	a.expToken()
	in := map[string]any{"plan": m.PlanID, "cell": *cell}
	var out fixtureAnswer
	if _, err := a.c.DoStatus(a.ctx, "POST", "/api/exp/fixtures/unseed", in, &out); err != nil {
		return err
	}
	if a.json {
		return a.printJSON(out)
	}
	if out.Removed == 0 {
		fmt.Fprintf(a.stdout, "nothing to remove for this cell\n")
		return nil
	}
	printFixtures(a, out.Fixtures)
	return nil
}

// cmdExpVerify compares the live fixtures with the checksums of the seeding.
func (a *app) cmdExpVerify(args []string) error {
	fs := a.flags("exp verify")
	path := fs.String("plan", "", "material directory with plan.yaml, or a plan file")
	cell := fs.String("cell", "", "cell of the plan")
	if _, err := a.parse(fs, args, 0, 0, "agw exp verify --plan <dir|file> --cell <id>"); err != nil {
		return err
	}
	if *path == "" || *cell == "" {
		return usagef("--plan and --cell are required")
	}
	m, err := loadMaterial(*path)
	if err != nil {
		return err
	}
	if _, err := m.cellOf(*cell); err != nil {
		return err
	}
	a.expToken()
	in := map[string]any{"plan": m.PlanID, "cell": *cell}
	var out fixtureAnswer
	status, err := a.c.DoStatus(a.ctx, "POST", "/api/exp/fixtures/verify", in, &out, 409)
	if err != nil {
		return err
	}
	if a.json {
		if err := a.printJSON(out); err != nil {
			return err
		}
	} else {
		switch {
		case out.State == "empty":
			// No fixture was examined; that is not a passed check (issue #149).
			fmt.Fprintf(a.stdout, "no fixtures recorded for this cell\n")
		case out.State == "mismatch":
			// The findings go to stderr below.
		case out.State == "" && out.Checked == 0 && len(out.Mismatches) == 0:
			// An older gateway without the state field: no mismatch, but nothing was checked either.
			fmt.Fprintf(a.stdout, "no fixtures recorded for this cell\n")
		default:
			fmt.Fprintf(a.stdout, "all %d fixtures match their checksums\n", out.Checked)
		}
	}
	if status == 409 || out.State == "mismatch" {
		printMismatches(a, out.Mismatches)
		return errSilentFailure
	}
	return nil
}

func printFixtures(a *app, rows []fixtureRow) {
	for _, row := range rows {
		state := "live"
		if row.RemovedAt != nil {
			state = "removed"
			if row.RemoveResult != "" {
				state += ": " + row.RemoveResult
			}
		}
		fmt.Fprintf(a.stdout, "%s  %s  %s  object %s  %s\n", row.ManifestID, row.Kind, row.Owner, row.ObjectID, state)
		names := make([]string, 0, len(row.FieldSHA256))
		for name := range row.FieldSHA256 {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(a.stdout, "    %-16s sha256 %s\n", name, row.FieldSHA256[name])
		}
	}
}

func printMismatches(a *app, list []mismatchRow) {
	for _, m := range list {
		if m.Missing {
			fmt.Fprintf(a.stderr, "fixture_mismatch: %s (%s %s): object is missing\n", m.ManifestID, m.Kind, m.ObjectID)
			continue
		}
		for _, f := range m.Fields {
			fmt.Fprintf(a.stderr, "fixture_mismatch: %s (%s %s): field %s expected sha256 %s, read back %s\n",
				m.ManifestID, m.Kind, m.ObjectID, f.Field, f.Expected, f.Actual)
		}
	}
}

// --- The runner (X1) -----------------------------------------------------------------------------

// expClient connects with the experiment key (X1.3). It is deliberately not AGW_API_TOKEN: the
// runner inside the gateway container must not carry the full key.
func (a *app) expClient() (*agwclient.Client, error) {
	token := os.Getenv("AGW_EXP_API_TOKEN")
	if token == "" {
		return nil, usagef("AGW_EXP_API_TOKEN is not set; the experiment routes need the key (X1.3)")
	}
	if a.c == nil {
		a.c = agwclient.New(a.url)
	}
	a.c.Token = token
	return a.c, nil
}

// multiFlag collects a switch that may be repeated (--cell).
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func (a *app) cmdExpRun(args []string) error {
	fs := a.flags("exp run")
	path := fs.String("plan", "", "material directory with plan.yaml, or a plan file")
	k := fs.Int("k", 0, "repetitions per task (default: the plan's k)")
	seed := fs.Int64("seed", 0, "seed of the order (default: the plan's seed)")
	dry := fs.Bool("dry-run", false, "print the order and change nothing")
	var cells multiFlag
	fs.Var(&cells, "cell", "cell to run (repeatable; default: every cell of the plan)")
	if _, err := a.parse(fs, args, 0, 0, "agw exp run --plan <dir|file> [--cell <id>]... [--k N] [--seed N] [--dry-run]"); err != nil {
		return err
	}
	if *path == "" {
		return usagef("--plan is required")
	}
	seedSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "seed" {
			seedSet = true
		}
	})
	m, err := loadMaterial(*path)
	if err != nil {
		return err
	}
	problems, err := materialProblems(m)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		for _, problem := range problems {
			fmt.Fprintln(a.stderr, problem.String())
		}
		return errSilentFailure
	}
	opts := expRunOptions{PlanPath: *path, Cells: cells, K: *k, Seed: *seed, SeedSet: seedSet, DryRun: *dry}
	if *dry {
		return runExpPlan(a.ctx, nil, opts, a.stdout, a.stderr)
	}
	c, err := a.expClient()
	if err != nil {
		return err
	}
	return runExpPlan(a.ctx, c, opts, a.stdout, a.stderr)
}

// cmdExpStatus shows how many runs of a plan exist per cell and status. A run that a newer attempt
// superseded counts as such, not in the outcome (X1.5).
func (a *app) cmdExpStatus(args []string) error {
	fs := a.flags("exp status")
	path := fs.String("plan", "", "material directory with plan.yaml, or a plan file")
	if _, err := a.parse(fs, args, 0, 0, "agw exp status --plan <dir|file> [--json]"); err != nil {
		return err
	}
	if *path == "" {
		return usagef("--plan is required")
	}
	m, err := loadMaterial(*path)
	if err != nil {
		return err
	}
	c, err := a.expClient()
	if err != nil {
		return err
	}
	runs, err := c.ExpRuns(a.ctx, m.PlanID, "")
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(runs)
	}
	type stats struct {
		runs, done, pass, started, limits, errors, aborted, superseded int
	}
	per := map[string]*stats{}
	get := func(cell string) *stats {
		if per[cell] == nil {
			per[cell] = &stats{}
		}
		return per[cell]
	}
	for _, run := range runs {
		s := get(run.Cell)
		if run.SupersededBy != "" {
			s.superseded++
			continue
		}
		s.runs++
		switch run.Status {
		case "done":
			s.done++
		case "started":
			s.started++
		case "error":
			s.errors++
		case "aborted":
			s.aborted++
		case "limit_cost", "limit_tokens", "limit_duration":
			s.limits++
		}
		if run.Pass != nil && *run.Pass {
			s.pass++
		}
	}
	cells := make([]string, 0, len(per))
	for cell := range per {
		cells = append(cells, cell)
	}
	sort.Strings(cells)
	tw := a.table()
	fmt.Fprintln(tw, "Cell\tRuns\tDone\tPass\tStarted\tLimits\tErrors\tAborted\tSuperseded")
	for _, cell := range cells {
		s := per[cell]
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", cell, s.runs, s.done, s.pass, s.started,
			s.limits, s.errors, s.aborted, s.superseded)
	}
	return tw.Flush()
}

// cmdExpAbort ends a run from outside: the chat is aborted and the run is closed as aborted. A run
// that already ended keeps its outcome.
func (a *app) cmdExpAbort(args []string) error {
	fs := a.flags("exp abort")
	pos, err := a.parse(fs, args, 1, 1, "agw exp abort <run>")
	if err != nil {
		return err
	}
	c, err := a.expClient()
	if err != nil {
		return err
	}
	run, err := c.ExpStopRun(a.ctx, pos[0])
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(run)
	}
	fmt.Fprintf(a.stdout, "run %s %s\n", run.Key, run.Status)
	return nil
}

// materialProblems checks the material of one plan: the format rules of X9 and, for a pilot, the
// separation from the main runs (X7.1).
func materialProblems(m *material) ([]exp.Problem, error) {
	problems := exp.Validate(m.Plan, m.Tasks, m.Manifests)
	if m.Plan != nil && m.Plan.Purpose == exp.PurposePilot {
		var mainTasks map[string]bool
		if filepath.Base(m.Path) != "plan.yaml" {
			var err error
			mainTasks, err = exp.MainTaskIDs(m.Dir)
			if err != nil {
				return nil, err
			}
		}
		problems = append(problems, exp.PilotProblems(m.Plan, m.Tasks, m.Manifests, mainTasks)...)
	}
	return problems, nil
}

// cmdExpValidate checks one plan against the formats of X9. --plan takes the material
// directory (then plan.yaml is read) or a plan file such as pilot.yaml.
func (a *app) cmdExpValidate(args []string) error {
	fs := a.flags("exp validate")
	path := fs.String("plan", "", "material directory with plan.yaml, or a plan file")
	if _, err := a.parse(fs, args, 0, 0, "agw exp validate --plan <dir|file>"); err != nil {
		return err
	}
	if *path == "" {
		return usagef("--plan is required")
	}
	m, err := loadMaterial(*path)
	if err != nil {
		return err
	}
	problems, err := materialProblems(m)
	if err != nil {
		return err
	}
	for _, problem := range problems {
		fmt.Fprintln(a.stdout, problem.String())
	}
	if len(problems) > 0 {
		return errSilentFailure
	}
	fmt.Fprintf(a.stdout, "%s: %d tasks, %d cells, %d fixtures, no problems\n",
		m.Plan.Purpose, len(m.Tasks), len(m.Plan.Cells), len(m.Manifests))
	return nil
}

// cmdExpExport writes the protocol of one plan as JSONL and CSV and a manifest with checksums (X5.7).
//
// --plan takes the plan name stored with the runs or the plan file itself (then its purpose names the plan and its
// models supply model_role). --format is a comma-separated list of jsonl and csv.
func (a *app) cmdExpExport(args []string) error {
	fs := a.flags("exp export")
	planArg := fs.String("plan", "", "plan name or plan file (plan.yaml)")
	planFile := fs.String("plan-file", "", "plan file for model roles and cell factors")
	out := fs.String("out", "", "directory for the export")
	format := fs.String("format", "jsonl,csv", "comma-separated: jsonl, csv")
	if _, err := a.parse(fs, args, 0, 0, "agw exp export --plan <name|file> --out <dir> [--format jsonl,csv]"); err != nil {
		return err
	}
	if *planArg == "" || *out == "" {
		return usagef("--plan and --out are required")
	}
	formats := map[string]bool{}
	for _, f := range strings.Split(*format, ",") {
		switch f = strings.TrimSpace(f); f {
		case "jsonl", "csv":
			formats[f] = true
		case "":
		default:
			return usagef("unknown format %q (jsonl, csv)", f)
		}
	}
	plan, materialPath := *planArg, *planFile
	planFromFile := false
	if materialPath == "" {
		if st, err := os.Stat(plan); err == nil {
			planFromFile = true
			materialPath = plan
			if st.IsDir() {
				materialPath = filepath.Join(plan, "plan.yaml")
			}
		}
	}
	var roles map[string]string
	var cells map[string]exp.Cell
	var planSHA string
	if materialPath != "" {
		m, err := loadMaterial(materialPath)
		if err != nil {
			return err
		}
		if raw, err := os.ReadFile(m.Path); err == nil {
			sum := sha256.Sum256(raw)
			planSHA = hex.EncodeToString(sum[:])
		}
		if planFromFile {
			// X1 records the plan under its file name without extension (plan or pilot); a separate --plan-file
			// leaves --plan as the stored name.
			plan = m.PlanID
		}
		roles = map[string]string{}
		for _, mod := range m.Plan.Models {
			roles[mod.ID] = mod.Role
		}
		cells = map[string]exp.Cell{}
		for _, c := range m.Plan.Cells {
			cells[c.ID] = c
		}
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	// The export takes its own key; without one it falls back to the API token (local use).
	if tok := os.Getenv("AGW_EXP_API_TOKEN"); tok != "" {
		a.c.Token = tok
	}
	files := make([]map[string]any, 0, len(agwExportNames)*2)
	version := ""
	for _, table := range agwExportNames {
		tbl, err := a.c.ExportTable(a.ctx, plan, table)
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		if tbl.Version != "" {
			version = tbl.Version
		}
		rows, err := readExportRows(tbl.Body)
		tbl.Body.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		if table == "runs" {
			enrichRuns(rows, roles, cells)
			if planSHA == "" {
				for _, row := range rows {
					if s, _ := row["plan_sha256"].(string); s != "" {
						planSHA = s
						break
					}
				}
			}
		}
		entry, err := writeExportFiles(*out, table, tbl.Columns, rows, formats)
		if err != nil {
			return err
		}
		files = append(files, entry...)
		fmt.Fprintf(a.stdout, "%s: %d rows\n", table, len(rows))
	}
	manifest := map[string]any{
		"gateway_version": version,
		"plan":            plan,
		"plan_sha256":     planSHA,
		"exported_at":     time.Now().UTC().Format(time.RFC3339),
		"synthetic":       false,
		"files":           files,
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(*out, "manifest.json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "manifest.json: %d files\n", len(files))
	return nil
}

// agwExportNames lists the files of the export in the order of the manifest (X5.7).
var agwExportNames = []string{"runs", "platform_calls", "tool_executions", "llm_calls", "reconcile", "fixtures", "syscalls", "turns"}

// readExportRows decodes a JSONL stream.
func readExportRows(r io.Reader) ([]map[string]any, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	out := []map[string]any{}
	for {
		var row map[string]any
		err := dec.Decode(&row)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
}

// enrichRuns fills model_role and the factors from plan.yaml when the server could not know them. A value the
// server already set wins.
func enrichRuns(rows []map[string]any, roles map[string]string, cells map[string]exp.Cell) {
	for _, row := range rows {
		cell, _ := row["cell"].(string)
		c, ok := cells[cell]
		if ok {
			// The plan is authoritative: the server only derives the factors from the cell identifier, which cannot
			// express a combined binding such as mcp-cli reliably.
			row["binding"] = c.Binding
			row["protection"] = c.Protection
			if c.Attack {
				row["attack"] = "attack"
			} else {
				row["attack"] = "none"
			}
		}
		if s, _ := row["model_role"].(string); s != "" {
			continue
		}
		model, _ := row["model"].(string)
		if role := roles[model]; role != "" {
			row["model_role"] = role
		}
	}
}

// writeExportFiles writes one table as JSONL and/or CSV and returns the manifest entries with row counts and
// checksums.
func writeExportFiles(dir, table string, columns []string, rows []map[string]any, formats map[string]bool) ([]map[string]any, error) {
	entries := []map[string]any{}
	if formats["jsonl"] {
		e, err := writeOneFile(filepath.Join(dir, table+".jsonl"), len(rows), func(w io.Writer) error {
			enc := json.NewEncoder(w)
			enc.SetEscapeHTML(false)
			for _, row := range rows {
				if err := enc.Encode(row); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		e["name"] = table + ".jsonl"
		entries = append(entries, e)
	}
	if formats["csv"] {
		e, err := writeOneFile(filepath.Join(dir, table+".csv"), len(rows), func(w io.Writer) error {
			cw := csv.NewWriter(w)
			if err := cw.Write(columns); err != nil {
				return err
			}
			for _, row := range rows {
				rec := make([]string, len(columns))
				for i, c := range columns {
					rec[i] = csvValue(row[c])
				}
				if err := cw.Write(rec); err != nil {
					return err
				}
			}
			cw.Flush()
			return cw.Error()
		})
		if err != nil {
			return nil, err
		}
		e["name"] = table + ".csv"
		entries = append(entries, e)
	}
	return entries, nil
}

// writeOneFile writes and hashes in one pass.
func writeOneFile(path string, rows int, write func(io.Writer) error) (map[string]any, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	if err := write(io.MultiWriter(f, h)); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return map[string]any{"rows": rows, "sha256": sumHex(h)}, nil
}

func sumHex(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

// csvValue renders a value for a spreadsheet; nested fields become JSON text.
func csvValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return fmt.Sprint(x)
	case json.Number:
		return x.String()
	case map[string]any, []any:
		b, _ := json.Marshal(x)
		return string(b)
	default:
		return fmt.Sprint(x)
	}
}
