// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agw/internal/agwclient"
	"agw/internal/exp"
)

// cmdExp dispatches the experiment subcommands: validate (X9), seed, unseed and verify of the
// fixtures (X4, issue #129), and run, status and abort of the runner (X1).
func (a *app) cmdExp(args []string) error {
	if len(args) == 0 {
		return usagef("exp: subcommand missing (validate, seed, unseed, verify, run, status, abort)")
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
	if len(out.Fixtures) == 0 {
		fmt.Fprintf(a.stdout, "no live fixtures for cell %s\n", *cell)
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
	} else if status != 409 {
		fmt.Fprintf(a.stdout, "cell %s: all fixtures match their checksums\n", *cell)
	}
	if status == 409 {
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
	problems := exp.Validate(m.Plan, m.Tasks, m.Manifests)
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
