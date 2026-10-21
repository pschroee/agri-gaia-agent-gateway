// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Experiment slots (X0): the action space of the outline, no subagents, no internet switch, the same
// rights description in every binding — and no word about the protection level.

func argTools(t *testing.T, args []string) []string {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--tools" {
			return strings.Split(args[i+1], ",")
		}
	}
	t.Fatalf("no --tools in %v", args)
	return nil
}

func hasPair(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

// X0.4: MCP gets the extension's tools and no file tools; the internet switch is gone as well.
func TestExperimentMCPHasNoFileTools(t *testing.T) {
	args, err := piArgs("exp:mcp", "p", "m", "note")
	if err != nil {
		t.Fatal(err)
	}
	tools := argTools(t, args)
	for _, forbidden := range []string{"read", "write", "ls", "mcp_request_internet", "mcp_disable_internet"} {
		if slices.Contains(tools, forbidden) {
			t.Errorf("experiment MCP has %q: %v", forbidden, tools)
		}
	}
	for _, want := range []string{"mcp_ping", "mcp_list_artifacts", "mcp_upload_artifact", "todo", "web_search", "web_extract"} {
		if !slices.Contains(tools, want) {
			t.Errorf("experiment MCP lost %q: %v", want, tools)
		}
	}
	if !hasPair(args, "--exclude-tools", "read,write,ls,mcp_request_internet,mcp_disable_internet") {
		t.Errorf("MCP exclusions missing: %v", args)
	}
	// The product slot keeps the file tools.
	product, _ := piArgs("mcp", "p", "m", "note")
	if !slices.Contains(argTools(t, product), "read") {
		t.Errorf("product MCP lost read: %v", argTools(t, product))
	}
}

// X0.6: no subagents in any experiment binding; the CLI keeps bash and its other tools.
func TestExperimentCLIHasNoSubagents(t *testing.T) {
	args, err := piArgs("exp:cli", "p", "m", "note")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, subagentsExt) || strings.Contains(joined, intercomExt) {
		t.Errorf("experiment CLI loads subagents or intercom: %v", args)
	}
	if !slices.Contains(args, "subagent,intercom") {
		t.Errorf("experiment CLI does not exclude subagent and intercom: %v", args)
	}
	if !slices.Contains(args, hiddenSubagentTools) {
		t.Errorf("experiment CLI lost the lazy-subagent switch: %v", args)
	}
	// The product slot still has both extensions.
	product, _ := piArgs("cli", "p", "m", "note")
	if !strings.Contains(strings.Join(product, " "), subagentsExt) {
		t.Errorf("product CLI lost the subagents: %v", product)
	}
}

// X0.5: REST stays as it is apart from the internet tools.
func TestExperimentAPIHasNoInternetTools(t *testing.T) {
	args, err := piArgs("exp:api", "p", "m", "note")
	if err != nil {
		t.Fatal(err)
	}
	tools := argTools(t, args)
	for _, forbidden := range []string{"request_internet", "disable_internet"} {
		if slices.Contains(tools, forbidden) {
			t.Errorf("experiment REST has %q: %v", forbidden, tools)
		}
	}
	if !slices.Contains(tools, "platform_http") || !slices.Contains(tools, "todo") {
		t.Errorf("experiment REST lost tools: %v", tools)
	}
	product, _ := piArgs("api", "p", "m", "note")
	if !slices.Contains(argTools(t, product), "request_internet") {
		t.Errorf("product REST lost request_internet")
	}
}

// X0.6: the system note has no paragraph on subagents and no line on requesting internet; both stay
// in the product note.
func TestExperimentNoteHasNoSubagentParagraph(t *testing.T) {
	note := SystemNoteFor("exp:cli", 4, nil)
	if strings.Contains(note, "start subagents") {
		t.Errorf("experiment note still describes subagents:\n%s", note)
	}
	if strings.Contains(note, "request it with a short reason") {
		t.Errorf("experiment note still asks for internet:\n%s", note)
	}
	if !strings.Contains(note, "- Task list:") {
		t.Errorf("experiment note lost the task list:\n%s", note)
	}
	product := SystemNoteFor("cli", 4, nil)
	if !strings.Contains(product, "start subagents") || !strings.Contains(product, "request it with a short reason") {
		t.Errorf("product note lost its paragraphs")
	}
}

// X0.11: every experiment binding loads the shared rights skill; the CLI gets platform-exp instead of
// platform.
func TestExperimentArgsLoadRightsSkill(t *testing.T) {
	for _, variant := range []string{"exp:cli", "exp:mcp", "exp:api", "exp:cli,api"} {
		args, err := PiArgs(variant, "p", "m")
		if err != nil {
			t.Fatalf("%s: %v", variant, err)
		}
		if !hasPair(args, "--skill", delegatedRightsSkill) {
			t.Errorf("%s: delegated-rights not loaded: %v", variant, args)
		}
		if strings.HasPrefix(variant, "exp:cli") {
			if !hasPair(args, "--skill", platformExpSkil) {
				t.Errorf("%s: platform-exp not loaded: %v", variant, args)
			}
			if hasPair(args, "--skill", platformSkil) {
				t.Errorf("%s: the product platform skill is loaded: %v", variant, args)
			}
		}
	}
	// The product slots keep platform and do not load the shared skill.
	args, _ := PiArgs("cli", "p", "m")
	if !hasPair(args, "--skill", platformSkil) || hasPair(args, "--skill", delegatedRightsSkill) {
		t.Errorf("product CLI skills changed: %v", args)
	}
}

// The model and the provider stay arguments; the profile does not pin DeepSeek or any other provider.
func TestExperimentKeepsConfiguredModelAndProvider(t *testing.T) {
	for _, variant := range []string{"exp:cli", "exp:mcp", "exp:api"} {
		args, err := piArgs(variant, "my-provider", "my-model", "note")
		if err != nil {
			t.Fatal(err)
		}
		if !hasPair(args, "--provider", "my-provider") || !hasPair(args, "--model", "my-model") {
			t.Errorf("%s: provider/model not passed through: %v", variant, args)
		}
	}
}

// X0.10: no skill an experiment slot loads carries a normative sentence about the protection level.
// platform is checked without the marked section, because that is what platform-exp is built from.
func TestExperimentSkillsHaveNoInstruction(t *testing.T) {
	forbidden := []string{"only logged", "violations are blocked", "authorization service", "outside your rights", "do not try another way"}
	check := func(name, text string) {
		for _, w := range forbidden {
			if strings.Contains(text, w) {
				t.Errorf("%s contains %q", name, w)
			}
		}
	}
	// Every fixed skill of an experiment CLI slot, except platform: its section is replaced while the
	// image is built.
	for _, imagePath := range fixedCLISkills {
		rel := strings.TrimPrefix(imagePath, "/opt/agw/skills/")
		text := repoFile(t, filepath.Join("images/agw-basis/skills", rel, "SKILL.md"))
		if imagePath == platformSkil {
			marked := regexp.MustCompile(`(?s)<!-- experiment:rights -->.*?<!-- /experiment:rights -->`)
			if !marked.MatchString(text) {
				t.Fatalf("platform/SKILL.md: experiment markers missing")
			}
			text = marked.ReplaceAllString(text, "")
		}
		check(rel, text)
	}
	check("delegated-rights", repoFile(t, "images/agw-basis/skills/delegated-rights/SKILL.md"))
}

// The shared skill has pi's front matter, a description within the limit and names the rights command
// of every binding.
func TestDelegatedRightsSkillFile(t *testing.T) {
	s := repoFile(t, "images/agw-basis/skills/delegated-rights/SKILL.md")
	fm := regexp.MustCompile(`(?s)\A---\nname: delegated-rights\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(s)
	if fm == nil {
		t.Fatalf("front matter missing or wrong:\n%.300s", s)
	}
	d := fm[1]
	if len(d) > 1024 || strings.Contains(d, ": ") || strings.Contains(d, " #") {
		t.Errorf("description too long (%d) or not a plain scalar: %s", len(d), d)
	}
	for _, w := range []string{"agw-platform rights", "mcp_platform_rights", "/_agw/rights"} {
		if !strings.Contains(s, w) {
			t.Errorf("skill does not name %q", w)
		}
	}
}

// The build script exists in both images and derives platform-exp from platform.
func TestPlatformExpIsGenerated(t *testing.T) {
	if _, err := os.Stat(filepath.Join("..", "..", "images", "agw-basis", "skills", "build-platform-exp.mjs")); err != nil {
		t.Fatal(err)
	}
	// One RUN per image; the script names itself once to run and once to remove.
	d := repoFile(t, "images/agw-basis/Dockerfile")
	if n := strings.Count(d, "RUN node /opt/agw/skills/build-platform-exp.mjs"); n != 2 {
		t.Errorf("build script runs in %d images, want 2 (pi and exec)", n)
	}
}
