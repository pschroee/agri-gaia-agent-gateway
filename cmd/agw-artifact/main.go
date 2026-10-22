// agw-artifact is the helper CLI in the sandbox. It speaks HTTP to the
// orchestrator over the slot's Unix socket. Which chat is meant is decided
// by the orchestrator solely from the socket the request arrives on; the CLI
// sends no information about it.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"agw/internal/execproto"
)

const defaultSocket = "/run/agw/agw.sock"

// Exit codes: 0 approved/successful, 1 error, 3 rejected by the user (internet, platform writes),
// 4 outside the delegated rights, 5 the capability is not part of this setup (G1.13).
const exitRejected = 3

// exitUsage: wrong usage, or a request the gateway refused (issue #175).
const exitUsage = 2

// ignoreSIGPIPE keeps the CLI alive when the reader of a pipe (`| head -3`) closes it early: without it the first
// write after that kills the process before it reports its exit code (#202, found live on `agw-platform nope 2>&1 | head -3`).
func ignoreSIGPIPE() { signal.Ignore(syscall.SIGPIPE) }

func main() {
	ignoreSIGPIPE()
	socket := os.Getenv("AGW_SOCKET")
	if socket == "" {
		socket = defaultSocket
	}
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}

	// Called as agw-platform (symlink in the image): Agri-Gaia platform.
	if filepath.Base(os.Args[0]) == "agw-platform" {
		code, err := platformCmd(hc, os.Args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			exit(hc, "agw-platform", 1)
		}
		exit(hc, "agw-platform", code)
	}
	// Called as agw-internet (symlink in the image): request internet access or switch it off.
	if filepath.Base(os.Args[0]) == "agw-internet" {
		code, err := internet(hc, os.Args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			exit(hc, "agw-internet", 1)
		}
		exit(hc, "agw-internet", code)
	}
	if len(os.Args) < 2 {
		usage()
		exit(hc, "agw-artifact", 1)
	}
	var err error
	code := 0
	switch os.Args[1] {
	case "internet":
		code, err = internet(hc, os.Args[2:])
	case "upload":
		code, err = upload(hc, os.Args[2:])
	case "list":
		err = list(hc)
	case "get":
		err = get(hc, os.Args[2:])
	case "platform":
		code, err = platformCmd(hc, os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		exit(hc, "agw-artifact", 1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		exit(hc, "agw-artifact", 1)
	}
	exit(hc, "agw-artifact", code)
}

func usage() {
	fmt.Fprint(os.Stderr, `agw-artifact — store artifacts of the current chat at the orchestrator

  agw-artifact upload <file> [--name <name>]    send a file inside /workspace to the user (shown in the chat at once)
  agw-artifact list                             list the artifacts of this chat
  agw-artifact get <name> [-o <file>]           download an artifact (user inputs: --kind input)
  agw-internet search|domains|packages|full …   request internet access of one kind; waits for the user (agw-internet --help)
  agw-internet status | off [<grant id>]        list the grants / end them (no approval)
  agw-platform <command> …                      Agri-Gaia platform (agw-platform --help)

Exit code for upload: 0 sent, 1 not sent (error).
`)
}

type uploadResult struct {
	Status  string `json:"status"` // upload: stored | rejected; internet: approved | rejected
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Message string `json:"message"`
	Text    string `json:"text"` // upload: the line for the agent
}

// upload sends a file to the user (issue #62): it is stored as an artifact of the chat at once, without approval, and
// appears in the chat as a file message of the agent. Only regular files inside /workspace, reached without symbolic
// links (opened with O_NOFOLLOW|O_NONBLOCK like the other helpers); the orchestrator checks size and name again.
func upload(hc *http.Client, args []string) (int, error) {
	file, name, err := parseUploadArgs(args)
	if err != nil {
		return 1, err
	}
	f, err := execproto.OpenInside(workspace, file)
	if err != nil {
		return 1, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return 1, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 1, err
	}
	sum := hex.EncodeToString(h.Sum(nil))

	req, _ := http.NewRequest(http.MethodPost, "http://agw/artifacts?name="+url.QueryEscape(name), f)
	req.ContentLength = size
	req.Header.Set("X-Agw-Sha256", sum)
	req.Header.Set("X-Agw-Via", "cli")
	setCaller(req)
	resp, err := hc.Do(req)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 1, fmt.Errorf("orchestrator responds %d: %s", resp.StatusCode, body)
	}
	var r uploadResult
	if err := json.Unmarshal(body, &r); err != nil {
		return 1, fmt.Errorf("unreadable response: %s", body)
	}
	return printUpload(os.Stdout, r), nil
}

// printUpload writes the result line and returns the exit code: 0 sent, 1 not sent.
func printUpload(w io.Writer, r uploadResult) int {
	text := r.Text
	ok := r.Status == "stored"
	if text == "" {
		if ok {
			text = fmt.Sprintf("sent: %q is in the chat for the user (%d bytes, sha256 %s)", r.Name, r.Size, r.SHA256)
		} else {
			msg := r.Message
			if msg == "" {
				msg = "not stored"
			}
			text = fmt.Sprintf("not sent: %q was not stored (%s)", r.Name, msg)
		}
	}
	fmt.Fprintln(w, text)
	if ok {
		return 0
	}
	return 1
}

// parseUploadArgs: upload <file> [--name <name>]; the file may also come after the flags. The name defaults to the
// file name.
func parseUploadArgs(args []string) (file, name string, err error) {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	n := fs.String("name", "", "name of the artifact (default: file name)")
	// The positional argument may come before the flags.
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		file, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", "", err
	}
	if file == "" && fs.NArg() > 0 {
		file = fs.Arg(0)
	}
	if file == "" {
		return "", "", errors.New("no file given")
	}
	name = *n
	if name == "" {
		name = filepath.Base(file)
	}
	return file, name, nil
}

// workspace is the directory files are sent from; tests replace it (no environment variable, so the agent cannot).
var workspace = execproto.Workspace

const internetHelp = `agw-internet asks the user for exactly the internet access a command needs and waits for the decision.
Ask for the narrowest kind; the user sees the kind, the list and the limits and approves exactly that.

  agw-internet search "<reason>"                          web_search and web_extract only (no commands)
  agw-internet domains <host[,host…]> "<reason>" [--minutes N] [--mb N]
                                                          https to exactly these host names (no wildcards,
                                                          at most 10); a redirect to another host is blocked
                                                          until you ask for that host too
  agw-internet packages <pip:name[==ver]|npm:name[@ver][,…]> "<reason>"
                                                          pip and npm packages, ends when they are installed
  agw-internet full "<reason>" [--minutes N] [--mb N]     everything, as the last resort
  agw-internet status                                     list the active grants with their limits
  agw-internet off [<grant id>]                           end one grant or all (no approval)

Time and data are limited: the gateway clamps --minutes and --mb to its bounds (defaults 30 min, 500 MB).
Use -- before a reason that starts with "-".
Exit code: 0 granted, 1 error, 2 wrong usage or refused by the gateway, 3 rejected by the user.
`

// Actions of agw-internet.
const (
	internetRequest = "request"
	internetOff     = "off"
	internetStatus  = "status"
	internetHelpCmd = "help"
)

// internetCmd is a parsed agw-internet command line.
type internetCmd struct {
	Action   string
	Kind     string
	Reason   string
	Domains  []string
	Packages []string
	Minutes  int
	MB       int
	GrantID  int64
}

var internetKinds = []string{"search", "domains", "packages", "full"}

// errOldForm is the answer to the form before issue #175, which asked for everything: it would become full access
// silently, so it is refused (exit code 2).
var errOldForm = errors.New(`the first argument must be one of search, domains, packages, full, status, off (the old form agw-internet "<reason>" is gone: ask for the narrowest kind, e.g. agw-internet domains download.example.org "<reason>")`)

// kindUsage is the usage line of each kind, shown with a usage error of that kind instead of the whole help.
var kindUsage = map[string]string{
	"search":   `agw-internet search "<reason>"`,
	"domains":  `agw-internet domains <host[,host…]> "<reason>" [--minutes N] [--mb N]`,
	"packages": `agw-internet packages <pip:name[==ver]|npm:name[@ver][,…]> "<reason>"`,
	"full":     `agw-internet full "<reason>" [--minutes N] [--mb N]`,
}

// hasSpaceOutsideBrackets reports white space in s that is not inside [...] (pip extras may say [a, b]).
func hasSpaceOutsideBrackets(s string) bool {
	depth := 0
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r == '[':
			depth++
		case r == ']' && depth > 0:
			depth--
		case unicode.IsSpace(r) && depth == 0:
			return true
		}
	}
	return false
}

// shortText cuts s to 60 characters for an error message.
func shortText(s string) string {
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return s
}

// listProblem names what is wrong with the first word of a domains or packages request ("" if it looks like a list):
// a sentence such as "pip install scikit-learn" is a reason or a command, not a list (issue #196).
func listProblem(kind, s string) string {
	switch kind {
	case "packages":
		if hasSpaceOutsideBrackets(s) || !strings.Contains(s, ":") {
			return fmt.Sprintf(`packages: "%s" is not a package list; write ecosystem:name without spaces, e.g. agw-internet packages pip:scikit-learn "<reason>"`, shortText(s))
		}
	case "domains":
		if len(strings.Fields(s)) > 1 {
			return fmt.Sprintf(`domains: "%s" is not a host list; write host names separated by commas, e.g. agw-internet domains download.example.org "<reason>"`, shortText(s))
		}
	}
	return ""
}

// parseInternetArgs reads a command line: a kind with its list and a reason, status, off [<grant id>] or help.
// --minutes and --mb may stand anywhere; everything after "--" is the reason.
func parseInternetArgs(args []string) (internetCmd, error) {
	var c internetCmd
	if len(args) == 0 {
		return c, errors.New("no kind given: " + strings.Join(internetKinds, ", "))
	}
	first := strings.ToLower(strings.TrimSpace(args[0]))
	rest := args[1:]
	switch first {
	case "-h", "--help", "help":
		c.Action = internetHelpCmd
		return c, nil
	case "status":
		if len(rest) > 0 {
			return c, errors.New(`"status" takes no arguments`)
		}
		c.Action = internetStatus
		return c, nil
	case "off":
		c.Action = internetOff
		switch len(rest) {
		case 0:
		case 1:
			n, err := strconv.ParseInt(strings.TrimSpace(rest[0]), 10, 64)
			if err != nil || n < 1 {
				return c, errors.New(`"off" takes an optional grant id (a positive number)`)
			}
			c.GrantID = n
		default:
			return c, errors.New(`"off" takes at most one argument, a grant id`)
		}
		return c, nil
	case "search", "domains", "packages", "full":
		c.Action, c.Kind = internetRequest, first
	default:
		return c, errOldForm
	}
	// Flags and words. After "--" every word belongs to the reason.
	var words []string
	for i := 0; i < len(rest); i++ {
		w := rest[i]
		if w == "--" {
			words = append(words, rest[i+1:]...)
			break
		}
		name, val, hasVal := strings.Cut(w, "=")
		if name == "--minutes" || name == "--mb" {
			if !hasVal {
				if i+1 >= len(rest) {
					return c, fmt.Errorf("%s needs a number", name)
				}
				i++
				val = rest[i]
			}
			n, err := strconv.Atoi(strings.TrimSpace(val))
			if err != nil || n < 1 {
				return c, fmt.Errorf("%s needs a positive number", name)
			}
			if name == "--minutes" {
				c.Minutes = n
			} else {
				c.MB = n
			}
			continue
		}
		words = append(words, w)
	}
	switch c.Kind {
	case "domains", "packages":
		if len(words) == 0 {
			return c, fmt.Errorf("%s needs a list first", c.Kind)
		}
		if msg := listProblem(c.Kind, words[0]); msg != "" {
			return c, errors.New(msg)
		}
		var list []string
		if c.Kind == "packages" {
			// The gateway splits the list itself: extras may contain commas (pip:torch[a,b]).
			if e := strings.TrimSpace(words[0]); e != "" {
				list = []string{e}
			}
		} else {
			for _, e := range strings.Split(words[0], ",") {
				if e = strings.TrimSpace(e); e != "" {
					list = append(list, e)
				}
			}
		}
		if len(list) == 0 {
			return c, fmt.Errorf("%s: the list is empty", c.Kind)
		}
		if c.Kind == "domains" {
			c.Domains = list
		} else {
			c.Packages = list
		}
		words = words[1:]
	}
	c.Reason = strings.TrimSpace(strings.Join(words, " "))
	if c.Reason == "" {
		return c, errors.New("no reason given: say in a few words what the access is for")
	}
	return c, nil
}

// internet asks the user for internet access and waits for the decision, lists the grants or ends them.
func internet(hc *http.Client, args []string) (int, error) {
	c, err := parseInternetArgs(args)
	if err != nil {
		if u, ok := kindUsage[c.Kind]; ok {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, "usage:", u)
			fmt.Fprintln(os.Stderr, "(agw-internet --help lists all kinds)")
			return exitUsage, nil
		}
		fmt.Fprint(os.Stderr, internetHelp)
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitUsage, nil
	}
	switch c.Action {
	case internetHelpCmd:
		fmt.Fprint(os.Stderr, internetHelp)
		return 0, nil
	case internetOff:
		return internetSwitchOff(hc, c.GrantID)
	case internetStatus:
		return internetShowStatus(hc)
	}
	body, _ := json.Marshal(map[string]any{"kind": c.Kind, "reason": c.Reason, "domains": c.Domains, "packages": c.Packages, "minutes": c.Minutes, "megabytes": c.MB})
	fmt.Fprintln(os.Stderr, "Requesting internet access ("+c.Kind+"). Waiting for the user's decision …")
	req, _ := http.NewRequest(http.MethodPost, "http://agw/internet", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	setCaller(req)
	resp, err := hc.Do(req)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusBadRequest {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		fmt.Println("refused:", e.Error)
		return exitUsage, nil
	}
	if resp.StatusCode != http.StatusOK {
		return 1, fmt.Errorf("orchestrator responds %d: %s", resp.StatusCode, raw)
	}
	var r uploadResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return 1, fmt.Errorf("unreadable response: %s", raw)
	}
	switch r.Status {
	case "approved":
		fmt.Println("approved:", r.Message)
		return 0, nil
	case "refused":
		fmt.Println("refused:", r.Message)
		return exitUsage, nil
	}
	fmt.Println("rejected:", r.Message)
	return exitRejected, nil
}

// internetSwitchOff ends one grant or all (POST /internet/off). Nothing active is a success.
func internetSwitchOff(hc *http.Client, grantID int64) (int, error) {
	var body io.Reader
	if grantID > 0 {
		b, _ := json.Marshal(map[string]int64{"grant_id": grantID})
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://agw/internet/off", body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
	var r uploadResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return 1, fmt.Errorf("unreadable response: %s", raw)
	}
	fmt.Printf("%s: %s\n", r.Status, r.Message)
	return 0, nil
}

// internetShowStatus lists the active grants (GET /internet): kind, list, time and data left.
func internetShowStatus(hc *http.Client) (int, error) {
	req, _ := http.NewRequest(http.MethodGet, "http://agw/internet", nil)
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
	var out struct {
		Grants []struct {
			ID        int64      `json:"id"`
			Kind      string     `json:"kind"`
			Domains   []string   `json:"domains"`
			MaxBytes  int64      `json:"max_bytes"`
			Bytes     int64      `json:"bytes"`
			Blocked   int        `json:"blocked"`
			ExpiresAt *time.Time `json:"expires_at"`
		} `json:"grants"`
		Blocked []refusalEntry `json:"blocked"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 1, fmt.Errorf("unreadable response: %s", raw)
	}
	if len(out.Grants) == 0 {
		fmt.Println("no internet access is active")
	}
	for _, g := range out.Grants {
		what := g.Kind
		if len(g.Domains) > 0 {
			what += " " + strings.Join(g.Domains, ", ")
		}
		left := ""
		if g.ExpiresAt != nil {
			left = fmt.Sprintf("until %s", g.ExpiresAt.UTC().Format("15:04"))
		}
		fmt.Printf("grant %d: %s %s, %d of %d MB used, %d blocked\n", g.ID, what, left, g.Bytes>>20, g.MaxBytes>>20, g.Blocked)
	}
	printRefusals(os.Stdout, out.Blocked)
	return 0, nil
}

// refusalEntry is one refusal in the answer of GET /internet.
type refusalEntry struct {
	Host    string    `json:"host"`
	Package string    `json:"package"`
	Reason  string    `json:"reason"`
	Denied  string    `json:"denied"`
	At      time.Time `json:"at"`
}

// printRefusals lists the last refusals with their full text; pip and npm show only the HTTP status.
func printRefusals(w io.Writer, rs []refusalEntry) {
	if len(rs) == 0 {
		return
	}
	fmt.Fprintln(w, "recent refusals:")
	for _, r := range rs {
		what := r.Host
		if r.Package != "" {
			switch r.Host {
			case "pypi.org":
				what = "pip:" + r.Package
			case "registry.npmjs.org":
				what = "npm:" + r.Package
			}
		}
		fmt.Fprintf(w, "  %s %s (%s) %s\n", r.At.Local().Format("15:04"), what, r.Reason, r.Denied)
	}
}

func list(hc *http.Client) error {
	req, _ := http.NewRequest(http.MethodGet, "http://agw/artifacts", nil)
	setCaller(req)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("orchestrator responds %d: %s", resp.StatusCode, body)
	}
	var items []struct {
		Name      string    `json:"name"`
		Size      int64     `json:"size"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(body, &items); err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Println("(no artifacts in this chat)")
	}
	for _, it := range items {
		fmt.Printf("%-40s %10d  %s\n", it.Name, it.Size, it.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

func get(hc *http.Client, args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	out := fs.String("o", "", "target file (default: name of the artifact)")
	kind := fs.String("kind", "output", "output (result) or input (input from the user)")
	var name string
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	if name == "" {
		return errors.New("no name given")
	}
	if *out == "" {
		*out = filepath.Base(name)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://agw/artifacts/"+url.PathEscape(name)+"?kind="+url.QueryEscape(*kind), nil)
	setCaller(req)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("orchestrator responds %d: %s", resp.StatusCode, body)
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		fmt.Printf("%s saved (%d bytes)\n", *out, n)
	}
	return err
}

// setCaller tells the orchestrator the tool call and session in which this command runs (the
// orchestrator puts them into the environment); only for showing who asked.
// reportExit tells the gateway that a CLI ends with a code other than 0, so that the failure stays visible when a
// pipe (`| tail`) replaces the exit code of the shell command (#202). It waits at most 2 s and ignores every error:
// a dead socket must not hold the CLI back.
func reportExit(hc *http.Client, cli string, code int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"cli": cli, "exit": code})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agw/cli/exit", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	setCaller(req)
	resp, err := hc.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	resp.Body.Close()
}

// exit reports a failed run, then ends the process.
func exit(hc *http.Client, cli string, code int) {
	if code != 0 {
		reportExit(hc, cli, code)
	}
	os.Exit(code)
}

func setCaller(req *http.Request) {
	if id := os.Getenv("PI_AGW_TOOL_CALL_ID"); id != "" {
		req.Header.Set("X-Agw-Tool-Call", id)
	}
	if s := os.Getenv("PI_AGW_SESSION"); s != "" {
		req.Header.Set("X-Agw-Session", s)
	}
}
