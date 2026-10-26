package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"agw/internal/platform"
	"agw/internal/toolset"
)

// platformCmd is agw-platform: the tools of the platform binding as
// subcommands, generated from the same table as the MCP tools. The CLI
// only sends the arguments; the orchestrator builds and checks the call.
// exitDenied: call outside the delegated rights, refused by the authorization service.
const exitDenied = 4

// exitUnavailable: the capability this call needs is not part of this setup (G1.13); the agent should
// work around it instead of repeating the call.
const exitUnavailable = 5

func platformCmd(hc *http.Client, args []string) (int, error) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		platformUsage(os.Stdout)
		if len(args) == 0 {
			return 1, nil
		}
		return 0, nil
	}
	tool, ok := platform.Lookup(args[0])
	if !ok {
		platformUsage(os.Stderr)
		return 1, fmt.Errorf("unknown command %q", args[0])
	}
	if len(args) > 1 && (args[1] == "-h" || args[1] == "--help") {
		toolUsage(os.Stdout, tool)
		return 0, nil
	}
	in, err := parseToolArgs(tool, args[1:], os.Stdin)
	if err != nil {
		toolUsage(os.Stderr, tool)
		return 1, err
	}
	body, _ := json.Marshal(in)
	if line := waitLine(tool, in); line != "" {
		fmt.Fprintln(os.Stderr, line)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://agw/platform/"+tool.Name, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	setCaller(req)
	resp, err := hc.Do(req)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 1, fmt.Errorf("orchestrator responds %d: %s", resp.StatusCode, raw)
	}
	var r platform.Result
	if err := json.Unmarshal(raw, &r); err != nil {
		return 1, fmt.Errorf("unreadable response: %s", raw)
	}
	fmt.Println(cliRender(r.Text()))
	switch r.Status {
	case "ok":
		return 0, nil
	case "rejected":
		return exitRejected, nil
	case "denied":
		return exitDenied, nil
	case platform.StatusUnavailable:
		return exitUnavailable, nil
	}
	return 1, nil
}

// parseToolArgs: required arguments and optional text arguments in order as positional arguments, all
// arguments also as --name value. JSON values (type object, body) directly, as
// @file or - for standard input; query as repeated --query k=v. A list (type
// array) takes all remaining positional arguments or is given by repeating the option.
// Relative file paths (Param.Path) become absolute, relative to the current directory.
func parseToolArgs(tool platform.Tool, args []string, stdin io.Reader) (map[string]any, error) {
	params := map[string]platform.Param{}
	var positional []platform.Param
	for _, p := range tool.Params {
		params[p.Name] = p
		if p.Required || p.Type == "string" {
			positional = append(positional, p)
		}
	}
	out := map[string]any{}
	set := func(p platform.Param, v string) error {
		if p.Name == "query" {
			k, val, ok := strings.Cut(v, "=")
			if !ok || k == "" {
				return fmt.Errorf("--query expects name=value, not %q", v)
			}
			q, _ := out["query"].(map[string]any)
			if q == nil {
				q = map[string]any{}
				out["query"] = q
			}
			q[k] = val
			return nil
		}
		if p.Path {
			if !filepath.IsAbs(v) {
				abs, err := filepath.Abs(v)
				if err != nil {
					return err
				}
				v = abs
			}
			v = filepath.Clean(v)
		}
		if p.Type == "array" {
			l, _ := out[p.Name].([]any)
			out[p.Name] = append(l, v)
			return nil
		}
		if p.Type != "object" && p.Name != "body" {
			out[p.Name] = v
			return nil
		}
		data := []byte(v)
		switch {
		case v == "-":
			b, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
			if err != nil {
				return err
			}
			data = b
		case strings.HasPrefix(v, "@"):
			b, err := os.ReadFile(v[1:])
			if err != nil {
				return err
			}
			data = b
		}
		var x any
		if err := json.Unmarshal(data, &x); err != nil {
			return fmt.Errorf("%s is not valid JSON: %v", p.Name, err)
		}
		out[p.Name] = x
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if name, ok := strings.CutPrefix(a, "--"); ok && name != "" {
			var val string
			if n, v, has := strings.Cut(name, "="); has {
				name, val = n, v
			} else {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--%s without a value", name)
				}
				i++
				val = args[i]
			}
			p, known := params[strings.ReplaceAll(name, "-", "_")]
			if !known {
				return nil, fmt.Errorf("unknown option --%s", name)
			}
			if err := set(p, val); err != nil {
				return nil, err
			}
			continue
		}
		var next *platform.Param
		for j := range positional {
			if _, done := out[positional[j].Name]; !done || positional[j].Type == "array" {
				next = &positional[j]
				break
			}
		}
		if next == nil {
			return nil, fmt.Errorf("surplus argument %q", a)
		}
		if err := set(*next, a); err != nil {
			return nil, err
		}
	}
	for _, p := range positional {
		if _, ok := out[p.Name]; !ok && p.Required {
			return nil, errors.New("argument " + p.Name + " is missing")
		}
	}
	return out, nil
}

// cliRender puts the command line forms in the tool references of a text (#200).
func cliRender(s string) string { return platform.Render(s, toolset.CLI, toolset.Of(toolset.CLI)) }

func platformUsage(w io.Writer) {
	fmt.Fprint(w, "agw-platform — Agri-Gaia platform through the orchestrator (the token is kept there, not here)\n\n")
	for _, t := range platform.Tools {
		fmt.Fprintf(w, "  agw-platform %s%s\n", t.CLI, t.Synopsis())
	}
	fmt.Fprint(w, cliRender("\nagw-platform <command> --help shows the description. Writing calls wait for confirmation\n"+
		"by the user. Exit code: 0 ok, 3 rejected by the user, 4 outside the delegated rights\n"+
		"({{tool:rights}} shows them), 1 error (including HTTP errors of the platform).\n"))
}

func toolUsage(w io.Writer, t platform.Tool) {
	t = t.RenderedFor(toolset.CLI, toolset.Of(toolset.CLI)) // tool references in the command line forms (#200)
	fmt.Fprintf(w, "agw-platform %s%s\n\n%s\n", t.CLI, t.Synopsis(), t.Desc)
	for _, p := range t.Params {
		req := ""
		if p.Required {
			req = ", required"
		}
		typ := p.Type
		if typ == "" {
			typ = "JSON"
		}
		fmt.Fprintf(w, "  --%-20s %s (%s%s)\n", p.Name, p.Desc, typ, req)
		if sj := p.SchemaJSON(); sj != "" {
			fmt.Fprintf(w, "    schema: %s\n", sj)
		}
	}
}

// waitLine is what the CLI says before a call that may wait: for a plan, that the gateway checks it first; for
// another writing call, that the gateway checks it and asks where it needs a confirmation (a step of an approved
// plan asks for none). A reading call says nothing (issue #225).
func waitLine(tool platform.Tool, in map[string]any) string {
	switch {
	case tool.Name == "propose_plan":
		return "Proposing the plan. The gateway checks it first; only a valid plan is shown to the user for approval …"
	case tool.Write || (tool.Name == "request" && strings.ToUpper(fmt.Sprint(in["method"])) != "GET"):
		return "Writing call. The gateway checks it and asks the user where it needs a confirmation …"
	}
	return ""
}
