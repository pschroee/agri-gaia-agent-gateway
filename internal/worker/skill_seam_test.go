// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"slices"
	"strings"
	"testing"
)

// A registered CLI skill is loaded for the cli binding after the fixed skills and not for the bindings
// that cannot read a skill of their own (G1.6).
func TestRegisteredCLISkill(t *testing.T) {
	registerCLISkill("/opt/agw/skills/seam")
	t.Cleanup(func() { cliSkills = cliSkills[:len(cliSkills)-1] })

	args, err := PiArgs("cli", "p", "m")
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Index(args, "/opt/agw/skills/seam")
	last := slices.Index(args, modelCardSkill)
	if got < 0 || last < 0 || got < last {
		t.Fatalf("registered skill not after the fixed ones: %v", args)
	}
	if args[got-1] != "--skill" {
		t.Fatalf("registered skill without --skill: %v", args)
	}
	// A combination with cli gets it as well.
	if a, _ := PiArgs("cli,api", "p", "m"); !slices.Contains(a, "/opt/agw/skills/seam") {
		t.Fatalf("cli,api: %v", a)
	}
	// The mcp binding loads its own skills only.
	a, _ := PiArgs("mcp", "p", "m")
	if slices.Contains(a, "/opt/agw/skills/seam") {
		t.Fatalf("mcp must not load a CLI skill: %v", a)
	}
	if !strings.Contains(strings.Join(a, " "), "--skill "+webSkill) {
		t.Fatalf("mcp lost its own skill: %v", a)
	}
}
