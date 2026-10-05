// agw-artifact ist das Hilfs-CLI in der Sandbox. Es spricht HTTP über den
// Unix-Socket des Platzes mit dem Orchestrator. Welcher Chat gemeint ist,
// entscheidet der Orchestrator allein daran, über welchen Socket die Anfrage
// kommt; das CLI sendet dazu keine Angabe.
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

// Exit-Codes: 0 bestätigt/erfolgreich, 1 Fehler, 3 vom Nutzer abgelehnt.
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

	// Als agw-platform aufgerufen (Symlink im Abbild): Agri-Gaia-Plattform.
	if filepath.Base(os.Args[0]) == "agw-platform" {
		code, err := platformCmd(hc, os.Args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "Fehler:", err)
			os.Exit(1)
		}
		os.Exit(code)
	}
	// Als agw-internet aufgerufen (Symlink im Abbild): Internetzugang erbitten.
	if filepath.Base(os.Args[0]) == "agw-internet" {
		code, err := internet(hc, os.Args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "Fehler:", err)
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
		fmt.Fprintln(os.Stderr, "Fehler:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `agw-artifact — Artefakte des aktuellen Chats beim Orchestrator ablegen

  agw-artifact upload <datei> [--name <name>]   hochladen; wartet auf Bestätigung durch den Nutzer
  agw-artifact list                             Artefakte dieses Chats auflisten
  agw-artifact get <name> [-o <datei>]          Artefakt herunterladen (Eingaben des Nutzers: --kind input)
  agw-internet "<Begründung>"                   Internetzugang erbitten; wartet auf den Nutzer
  agw-platform <befehl> …                       Agri-Gaia-Plattform (agw-platform --help)

Exit-Code bei upload: 0 bestätigt, 3 abgelehnt, 1 Fehler.
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
	name := fs.String("name", "", "Name des Artefakts (Standard: Dateiname)")
	// Positionsargument darf vor den Flags stehen.
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
		return 1, errors.New("keine Datei angegeben")
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
	fmt.Fprintf(os.Stderr, "Lade %s hoch (%d Bytes). Warte auf Bestätigung durch den Nutzer …\n", *name, size)
	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 1, fmt.Errorf("Orchestrator antwortet %d: %s", resp.StatusCode, body)
	}
	var r uploadResult
	if err := json.Unmarshal(body, &r); err != nil {
		return 1, fmt.Errorf("unlesbare Antwort: %s", body)
	}
	wait := time.Since(start).Round(time.Second)
	switch r.Status {
	case "approved":
		fmt.Printf("bestätigt: Artefakt %q gespeichert (%d Bytes, sha256 %s, Wartezeit %s)\n", r.Name, r.Size, r.SHA256, wait)
		return 0, nil
	default:
		msg := r.Message
		if msg == "" {
			msg = "vom Nutzer abgelehnt"
		}
		fmt.Printf("abgelehnt: Artefakt %q wurde nicht gespeichert (%s, Wartezeit %s)\n", r.Name, msg, wait)
		return exitRejected, nil
	}
}

// internet bittet den Nutzer um Internetzugang und wartet auf die Entscheidung.
func internet(hc *http.Client, args []string) (int, error) {
	reason := strings.TrimSpace(strings.Join(args, " "))
	if reason == "" || reason == "-h" || reason == "--help" {
		fmt.Fprint(os.Stderr, "agw-internet \"<Begründung>\" — bittet den Nutzer um Internetzugang und wartet auf die Entscheidung.\nExit-Code: 0 freigegeben, 3 abgelehnt, 1 Fehler.\n")
		if reason == "" {
			return 1, nil
		}
		return 0, nil
	}
	body, _ := json.Marshal(map[string]string{"reason": reason})
	fmt.Fprintln(os.Stderr, "Bitte um Internetzugang. Warte auf die Entscheidung des Nutzers …")
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
		return 1, fmt.Errorf("Orchestrator antwortet %d: %s", resp.StatusCode, raw)
	}
	var r uploadResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return 1, fmt.Errorf("unlesbare Antwort: %s", raw)
	}
	if r.Status == "approved" {
		fmt.Println("bestätigt:", r.Message)
		return 0, nil
	}
	fmt.Println("abgelehnt:", r.Message)
	return exitRejected, nil
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
		return fmt.Errorf("Orchestrator antwortet %d: %s", resp.StatusCode, body)
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
		fmt.Println("(keine Artefakte in diesem Chat)")
	}
	for _, it := range items {
		fmt.Printf("%-40s %10d  %s\n", it.Name, it.Size, it.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

func get(hc *http.Client, args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	out := fs.String("o", "", "Zieldatei (Standard: Name des Artefakts)")
	kind := fs.String("kind", "output", "output (Ergebnis) oder input (Eingabe des Nutzers)")
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
		return errors.New("kein Name angegeben")
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
		return fmt.Errorf("Orchestrator antwortet %d: %s", resp.StatusCode, body)
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
		fmt.Printf("%s gespeichert (%d Bytes)\n", *out, n)
	}
	return err
}

// setCaller nennt dem Orchestrator Werkzeugaufruf und Sitzung, in denen dieser Befehl läuft (setzt der
// Orchestrator in die Umgebung); nur für die Anzeige, wer gefragt hat.
func setCaller(req *http.Request) {
	if id := os.Getenv("PI_AGW_TOOL_CALL_ID"); id != "" {
		req.Header.Set("X-Agw-Tool-Call", id)
	}
	if s := os.Getenv("PI_AGW_SESSION"); s != "" {
		req.Header.Set("X-Agw-Session", s)
	}
}
