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
	"path/filepath"
	"strings"
	"time"
)

const defaultSocket = "/run/agw/agw.sock"

// Exit codes: 0 approved/successful, 1 error, 3 rejected by the user.
const exitRejected = 3

func main() {
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
			os.Exit(1)
		}
		os.Exit(code)
	}
	// Called as agw-internet (symlink in the image): request internet access or switch it off.
	if filepath.Base(os.Args[0]) == "agw-internet" {
		code, err := internet(hc, os.Args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		os.Exit(code)
	}
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
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
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `agw-artifact — store artifacts of the current chat at the orchestrator

  agw-artifact upload <file> [--name <name>]    upload; waits for confirmation by the user
  agw-artifact list                             list the artifacts of this chat
  agw-artifact get <name> [-o <file>]           download an artifact (user inputs: --kind input)
  agw-internet "<reason>"                       request internet access; waits for the user
  agw-internet off                              switch internet access off again (no approval)
  agw-platform <command> …                      Agri-Gaia platform (agw-platform --help)

Exit code for upload: 0 approved, 3 rejected, 1 error.
`)
}

type uploadResult struct {
	Status  string `json:"status"` // approved | rejected
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Message string `json:"message"`
}

func upload(hc *http.Client, args []string) (int, error) {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	name := fs.String("name", "", "name of the artifact (default: file name)")
	// The positional argument may come before the flags.
	var file string
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		file, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 1, err
	}
	if file == "" && fs.NArg() > 0 {
		file = fs.Arg(0)
	}
	if file == "" {
		return 1, errors.New("no file given")
	}
	if *name == "" {
		*name = filepath.Base(file)
	}
	f, err := os.Open(file)
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

	req, _ := http.NewRequest(http.MethodPost, "http://agw/artifacts?name="+url.QueryEscape(*name), f)
	req.ContentLength = size
	req.Header.Set("X-Agw-Sha256", sum)
	req.Header.Set("X-Agw-Via", "cli")
	setCaller(req)
	fmt.Fprintf(os.Stderr, "Uploading %s (%d bytes). Waiting for confirmation by the user …\n", *name, size)
	start := time.Now()
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
	wait := time.Since(start).Round(time.Second)
	switch r.Status {
	case "approved":
		fmt.Printf("approved: artifact %q stored (%d bytes, sha256 %s, wait %s)\n", r.Name, r.Size, r.SHA256, wait)
		return 0, nil
	default:
		msg := r.Message
		if msg == "" {
			msg = "rejected by the user"
		}
		fmt.Printf("rejected: artifact %q was not stored (%s, wait %s)\n", r.Name, msg, wait)
		return exitRejected, nil
	}
}

const internetHelp = `agw-internet "<reason>"   asks the user for internet access and waits for the decision.
                          Exit code: 0 granted, 3 rejected, 1 error.
agw-internet off          switches internet access off again; needs no approval and succeeds
                          also when it is already off. Exit code: 0 off, 1 error.
agw-internet -- <reason>  requests with a reason that is literally "off" or starts with "-".
`

// Actions of agw-internet.
const (
	internetRequest = "request"
	internetOff     = "off"
	internetHelpCmd = "help"
)

// parseInternetArgs decides between request and switching off. "off" as the only argument (any case,
// surrounding spaces ignored) switches off; everything after "--" is a reason, so a reason that is
// literally "off" can still be given. "off" with further arguments is refused instead of guessed: it
// could be meant either way.
func parseInternetArgs(args []string) (action, reason string, err error) {
	if len(args) > 0 && args[0] == "--" {
		reason = strings.TrimSpace(strings.Join(args[1:], " "))
		if reason == "" {
			return "", "", errors.New("no reason given after --")
		}
		return internetRequest, reason, nil
	}
	if len(args) == 0 {
		return "", "", errors.New("no reason given")
	}
	first := strings.TrimSpace(args[0])
	switch {
	case len(args) == 1 && (first == "-h" || first == "--help"):
		return internetHelpCmd, "", nil
	case strings.EqualFold(first, internetOff):
		if len(args) > 1 {
			return "", "", errors.New(`"off" takes no further arguments; to request internet with a reason starting with "off", use: agw-internet -- <reason>`)
		}
		return internetOff, "", nil
	}
	reason = strings.TrimSpace(strings.Join(args, " "))
	if reason == "" {
		return "", "", errors.New("no reason given")
	}
	return internetRequest, reason, nil
}

// internet asks the user for internet access and waits for the decision, or switches it off.
func internet(hc *http.Client, args []string) (int, error) {
	action, reason, err := parseInternetArgs(args)
	if err != nil {
		fmt.Fprint(os.Stderr, internetHelp)
		return 1, err
	}
	switch action {
	case internetHelpCmd:
		fmt.Fprint(os.Stderr, internetHelp)
		return 0, nil
	case internetOff:
		return internetSwitchOff(hc)
	}
	body, _ := json.Marshal(map[string]string{"reason": reason})
	fmt.Fprintln(os.Stderr, "Requesting internet access. Waiting for the user's decision …")
	req, _ := http.NewRequest(http.MethodPost, "http://agw/internet", bytes.NewReader(body))
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
	var r uploadResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return 1, fmt.Errorf("unreadable response: %s", raw)
	}
	if r.Status == "approved" {
		fmt.Println("approved:", r.Message)
		return 0, nil
	}
	fmt.Println("rejected:", r.Message)
	return exitRejected, nil
}

// internetSwitchOff switches internet access off (POST /internet/off). Already off is a success.
func internetSwitchOff(hc *http.Client) (int, error) {
	req, _ := http.NewRequest(http.MethodPost, "http://agw/internet/off", nil)
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
func setCaller(req *http.Request) {
	if id := os.Getenv("PI_AGW_TOOL_CALL_ID"); id != "" {
		req.Header.Set("X-Agw-Tool-Call", id)
	}
	if s := os.Getenv("PI_AGW_SESSION"); s != "" {
		req.Header.Set("X-Agw-Session", s)
	}
}
