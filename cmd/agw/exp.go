// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os"

	"agw/internal/exp"
)

// cmdExp dispatches the experiment subcommands. X9 adds only validation; the runner follows in
// X1 and the fixture seeding in X4.
func (a *app) cmdExp(args []string) error {
	if len(args) == 0 {
		return usagef("exp: subcommand missing (validate)")
	}
	switch args[0] {
	case "validate":
		return a.cmdExpValidate(args[1:])
	}
	return usagef("unknown command \"exp %s\" – agw help shows all commands", args[0])
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
	var (
		p         *exp.Plan
		tasks     []exp.Task
		manifests []exp.Manifest
		err       error
	)
	if st, statErr := os.Stat(*path); statErr == nil && st.IsDir() {
		p, tasks, manifests, err = exp.LoadPlan(*path)
	} else {
		p, tasks, manifests, err = exp.LoadPlanFile(*path)
	}
	if err != nil {
		return err
	}
	problems := exp.Validate(p, tasks, manifests)
	for _, problem := range problems {
		fmt.Fprintln(a.stdout, problem.String())
	}
	if len(problems) > 0 {
		return errSilentFailure
	}
	fmt.Fprintf(a.stdout, "%s: %d tasks, %d cells, %d fixtures, no problems\n",
		p.Purpose, len(tasks), len(p.Cells), len(manifests))
	return nil
}
