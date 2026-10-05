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
)

// platformCmd ist agw-platform: die Werkzeuge der Plattform-Anbindung als
// Unterbefehle, erzeugt aus derselben Tabelle wie die MCP-Werkzeuge. Das CLI
// schickt nur die Argumente; den Aufruf baut und prüft der Orchestrator.
// exitDenied: Aufruf außerhalb der übertragenen Rechte, vom Autorisierungsdienst abgewiesen.
const exitDenied = 4

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
		return 1, fmt.Errorf("unbekannter Befehl %q", args[0])
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
	if tool.Write || (tool.Name == "request" && strings.ToUpper(fmt.Sprint(in["method"])) != "GET") {
		fmt.Fprintln(os.Stderr, "Schreibender Aufruf. Warte auf die Bestätigung durch den Nutzer …")
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
		return 1, fmt.Errorf("Orchestrator antwortet %d: %s", resp.StatusCode, raw)
	}
	var r platform.Result
	if err := json.Unmarshal(raw, &r); err != nil {
		return 1, fmt.Errorf("unlesbare Antwort: %s", raw)
	}
	fmt.Println(r.Text())
	switch r.Status {
	case "ok":
		return 0, nil
	case "rejected":
		return exitRejected, nil
	case "denied":
		return exitDenied, nil
	}
	return 1, nil
}

// parseToolArgs: Pflichtargumente und optionale Textargumente der Reihe nach als Positionsargumente, alle
// Argumente auch als --name wert. JSON-Werte (Typ object, body) direkt, als
// @datei oder - für die Standardeingabe; query als wiederholtes --query k=v. Eine Liste (Typ
// array) nimmt alle übrigen Positionsargumente auf oder wird mit der Option wiederholt angegeben.
// Relative Dateipfade (Param.Path) werden absolut, bezogen auf das aktuelle Verzeichnis.
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
				return fmt.Errorf("--query erwartet name=wert, nicht %q", v)
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
			return fmt.Errorf("%s ist kein gültiges JSON: %v", p.Name, err)
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
					return nil, fmt.Errorf("--%s ohne Wert", name)
				}
				i++
				val = args[i]
			}
			p, known := params[strings.ReplaceAll(name, "-", "_")]
			if !known {
				return nil, fmt.Errorf("unbekannte Option --%s", name)
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
			return nil, fmt.Errorf("überzähliges Argument %q", a)
		}
		if err := set(*next, a); err != nil {
			return nil, err
		}
	}
	for _, p := range positional {
		if _, ok := out[p.Name]; !ok && p.Required {
			return nil, errors.New("Argument " + p.Name + " fehlt")
		}
	}
	return out, nil
}

func platformUsage(w io.Writer) {
	fmt.Fprint(w, "agw-platform — Agri-Gaia-Plattform über den Orchestrator (Token liegt dort, nicht hier)\n\n")
	for _, t := range platform.Tools {
		fmt.Fprintf(w, "  agw-platform %s%s\n", t.CLI, synopsis(t))
	}
	fmt.Fprint(w, "\nagw-platform <befehl> --help zeigt die Beschreibung. Schreibende Aufrufe warten auf die Bestätigung\n"+
		"durch den Nutzer. Exit-Code: 0 ok, 3 vom Nutzer abgelehnt, 4 außerhalb der übertragenen Rechte\n"+
		"(agw-platform rights zeigt sie), 1 Fehler (auch HTTP-Fehler der Plattform).\n")
}

func toolUsage(w io.Writer, t platform.Tool) {
	fmt.Fprintf(w, "agw-platform %s%s\n\n%s\n", t.CLI, synopsis(t), t.Desc)
	for _, p := range t.Params {
		req := ""
		if p.Required {
			req = ", Pflicht"
		}
		typ := p.Type
		if typ == "" {
			typ = "JSON"
		}
		fmt.Fprintf(w, "  --%-20s %s (%s%s)\n", p.Name, p.Desc, typ, req)
	}
}

func synopsis(t platform.Tool) string {
	var b strings.Builder
	afterList := false // nach einer Liste geht alles Weitere nur als Option
	for _, p := range t.Params {
		switch {
		case p.Required && p.Type == "array":
			fmt.Fprintf(&b, " <%s …>", p.Name)
			afterList = true
		case p.Required:
			fmt.Fprintf(&b, " <%s>", p.Name)
		case p.Type == "string" && !afterList:
			fmt.Fprintf(&b, " [%s]", p.Name)
		case p.Name == "query":
			b.WriteString(" [--query k=v …]")
		case p.Type == "array":
			fmt.Fprintf(&b, " [--%s x …]", p.Name)
		default:
			fmt.Fprintf(&b, " [--%s …]", p.Name)
		}
	}
	return b.String()
}
