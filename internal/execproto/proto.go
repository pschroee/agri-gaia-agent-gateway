// Package execproto ist das Protokoll zwischen Orchestrator und dem Helfer
// agw-exec in der Ausführungs-Sandbox (E9). Beide Seiten tauschen JSON-Zeilen:
// Der Orchestrator schickt Anfragen, agw-exec antwortet mit Rahmen derselben
// Kennung. Ein Befehl (bash) liefert beliebig viele Rahmen mit Daten und zum
// Schluss einen Rahmen mit Done; alle anderen Operationen nur den letzten.
package execproto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Operationen. Jede läuft in der Ausführungs-Sandbox als Agent-Nutzer.
const (
	OpRead      = "read"       // Datei lesen (nur reguläre Dateien, höchstens Max Bytes)
	OpWrite     = "write"      // Datei schreiben (Data)
	OpMkdir     = "mkdir"      // Verzeichnis samt Eltern anlegen
	OpStat      = "stat"       // Angaben zu einem Pfad (folgt Symlinks)
	OpReaddir   = "readdir"    // Einträge eines Verzeichnisses samt Art
	OpAccess    = "access"     // lesbar (Mode "r") bzw. les- und schreibbar ("rw")
	OpImageType = "image_type" // Bildtyp aus den Magic Bytes
	OpBash      = "bash"       // Befehl mit bash -c, Ausgabe gestreamt
	OpBg        = "bg"         // Hintergrundaufgabe: wie bash, Ausgabe zusätzlich immer in /tmp/agw-bg/bg-<n>.log
	OpGrep      = "grep"       // Suche mit ripgrep
	OpGlob      = "glob"       // Dateien und Verzeichnisse nach Muster (fd, wie pis find)
	OpReadLines = "read_lines" // Ausschnitt einer großen Textdatei zeilenweise (read über MaxFileBytes)
	OpWorkflow  = "workflow"   // Skript eines Workflows (pi-subagents) in eigenem Node-Prozess
	OpInput     = "input"      // weitere Eingabe an eine laufende Operation (nur workflow)
	OpCancel    = "cancel"     // laufende Operation mit derselben ID abbrechen
)

// Ops sind alle Operationen außer cancel.
var Ops = []string{OpRead, OpWrite, OpMkdir, OpStat, OpReaddir, OpAccess, OpImageType, OpBash, OpBg, OpGrep, OpGlob, OpReadLines, OpWorkflow}

// Grenzen, die beide Seiten durchsetzen.
const (
	MaxFileBytes  = 64 << 20 // read und write (Artefakte bis AGW_ARTIFACT_MAX_MB, Standard 50)
	MaxPath       = 4096
	MaxCommand    = 256 << 10
	MaxFrameBytes = 96 << 20 // eine JSON-Zeile (base64 einer Datei an der Grenze passt hinein)
	MaxGrepLimit  = 1000
	MaxGlobLimit  = 100000
	// MaxTimeoutSec wie pi: setTimeout nimmt höchstens 2^31-1 ms (L1).
	MaxTimeoutSec = 2147483.647
	// MaxSpillBytes: So viel einer Befehlsausgabe landet höchstens in der Datei mit der ganzen
	// Ausgabe (/tmp/pi-bash-*.log in der Ausführungs-Sandbox, H1).
	MaxSpillBytes = 256 << 20
	// Schwellen von pi (DEFAULT_MAX_BYTES, DEFAULT_MAX_LINES): darunter keine Datei.
	PiMaxBytes        = 50 << 10
	PiMaxLines        = 2000
	MaxWorkflowSource = 4 << 20
	// MaxBgLogBytes: so viel der Ausgabe einer Hintergrundaufgabe landet höchstens in ihrer Datei.
	MaxBgLogBytes = 256 << 20
	// DefaultBgMax: so viele Hintergrundaufgaben laufen höchstens gleichzeitig je Platz (AGW_BG_MAX).
	DefaultBgMax = 5
	// BgThrottleAfter, BgThrottleRate: Nach so viel Ausgabe liest agw-exec eine Hintergrundaufgabe
	// nur noch mit dieser Rate (Bytes je Sekunde); der Befehl wartet dann beim Schreiben. So bleibt
	// die Last im Orchestrator (Prüfsumme, Mitlesen) begrenzt (Review 3, N3).
	BgThrottleAfter = 64 << 20
	BgThrottleRate  = 4 << 20
)

// BgLogRe: der einzige zulässige Pfad für die Ausgabe einer Hintergrundaufgabe. Der Orchestrator
// bildet ihn aus der laufenden Nummer der Aufgabe im Chat (BgLogPath).
var BgLogRe = regexp.MustCompile(`^/tmp/agw-bg/bg-[1-9][0-9]{0,8}\.log$`)

// BgLogDir enthält die Ausgaben der Hintergrundaufgaben in der Ausführungs-Sandbox.
const BgLogDir = "/tmp/agw-bg"

// BgLogPath: /tmp/agw-bg/bg-<n>.log
func BgLogPath(seq int) string { return fmt.Sprintf("%s/bg-%d.log", BgLogDir, seq) }

// SpillRe: der einzige zulässige Pfad für die ganze Ausgabe eines Befehls. Der Orchestrator
// bildet ihn aus der toolCallId (SpillPath), der Agent kann ihn nicht wählen.
var SpillRe = regexp.MustCompile(`^/tmp/pi-bash-[0-9a-f]{16}\.log$`)

// SpillPath: /tmp/pi-bash-<erste 16 Hex-Zeichen von sha256(toolCallId)>.log
func SpillPath(toolCallID string) string {
	sum := sha256.Sum256([]byte(toolCallID))
	return "/tmp/pi-bash-" + hex.EncodeToString(sum[:])[:16] + ".log"
}

type Request struct {
	ID      uint64            `json:"id"`
	Op      string            `json:"op"`
	Path    string            `json:"path,omitempty"`
	Data    []byte            `json:"data,omitempty"`
	Max     int64             `json:"max,omitempty"`
	Mode    string            `json:"mode,omitempty"`
	Command string            `json:"command,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout float64           `json:"timeout,omitempty"` // Sekunden, 0 = keine
	Grep    *GrepArgs         `json:"grep,omitempty"`
	Glob    *GlobArgs         `json:"glob,omitempty"`
	Lines   *LinesArgs        `json:"lines,omitempty"`
	// Spill: Datei für die ganze Ausgabe von bash (SpillRe) bzw. einer Hintergrundaufgabe (BgLogRe);
	// setzt der Orchestrator.
	Spill string `json:"spill,omitempty"`
	// LogFD setzt nur der Überwacher für seinen Helfer: Die Ausgabedatei einer Hintergrundaufgabe
	// ist schon offen (Deskriptor 3), angelegt vom Überwacher als root (Review 3, N1). Was der
	// Orchestrator hier schickt, überschreibt der Überwacher.
	LogFD bool `json:"logFd,omitempty"`
}

// LinesArgs: Zeilen Start (0-basiert) bis Start+Count-1, höchstens etwas mehr als MaxBytes.
type LinesArgs struct {
	Start    int64 `json:"start"`
	Count    int   `json:"count"`
	MaxBytes int   `json:"maxBytes"`
	// Select: so viele Zeilen ab Start hat der Aufrufer ausgewählt (0: bis zum Ende). Dafür
	// liefert readLines Umfang und Zeilenzahl, wie pis truncateHead sie über der Auswahl zählt.
	Select int64 `json:"select,omitempty"`
}

// LinesResult: Zeilen als Bytes (ohne „\n“), Gesamtzahl wie JavaScripts split("\n") und die
// Länge der ersten verlangten Zeile, auch wenn sie nur gekürzt in Lines steht.
type LinesResult struct {
	Lines          [][]byte `json:"lines"`
	TotalLines     int64    `json:"totalLines"`
	StartLineBytes int64    `json:"startLineBytes"`
	// Auswahl (Select): Bytes samt Zeilenumbrüchen, Zeilen, letzte Zeile leer.
	SelBytes     int64 `json:"selBytes"`
	SelLines     int64 `json:"selLines"`
	SelLastEmpty bool  `json:"selLastEmpty"`
}

type GrepArgs struct {
	Pattern    string `json:"pattern"`
	Glob       string `json:"glob,omitempty"`
	IgnoreCase bool   `json:"ignoreCase,omitempty"`
	Literal    bool   `json:"literal,omitempty"`
	Context    int    `json:"context,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

type GlobArgs struct {
	Pattern string `json:"pattern"`
	Limit   int    `json:"limit,omitempty"`
}

// Frame ist eine Antwortzeile. Data trägt Ausgabe eines Befehls, Done beendet
// die Operation (mit Exit bei bash, Result bei den übrigen oder Error).
type Frame struct {
	ID     uint64          `json:"id"`
	Data   []byte          `json:"data,omitempty"`
	Done   bool            `json:"done,omitempty"`
	Exit   *int            `json:"exit,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"` // ENOENT, EISDIR, EACCES, timeout, aborted, …
	Result json.RawMessage `json:"result,omitempty"`
	// Nur bash: Datei mit der ganzen Ausgabe (über pis Schwellen), in der Ausführungs-Sandbox.
	FullOutputPath string `json:"fullOutputPath,omitempty"`
	// Nur bg: Prozessgruppe des Befehls, im ersten Rahmen gemeldet. Endet der Helfer ohne
	// Ergebnis (etwa weil der Agent ihn beendet hat), beendet der Überwacher diese Gruppe.
	Pgid       int    `json:"pgid,omitempty"`
	SpillError string `json:"spillError,omitempty"`
	// Nur bash am Socket von pi, Code "backgrounded": Der Nutzer hat den Befehl in eine
	// Hintergrundaufgabe umgewandelt; ihre Kennung (bg-<n>).
	Background string `json:"background,omitempty"`
}

type ReadResult struct {
	Data []byte `json:"data"`
	Size int64  `json:"size"`
}

type StatResult struct {
	Exists  bool   `json:"exists"`
	IsDir   bool   `json:"isDir"`
	IsFile  bool   `json:"isFile"`
	Size    int64  `json:"size"`
	MtimeMs int64  `json:"mtimeMs"`
	Mode    uint32 `json:"mode"`
}

type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
}

type ReaddirResult struct {
	Entries []DirEntry `json:"entries"`
}

type ImageTypeResult struct {
	Mime string `json:"mime"`
}

// GrepLine ist eine Zeile der Ausgabe von grep: Treffer oder Kontext.
type GrepLine struct {
	Path  string `json:"path"` // wie pi: relativ zum Suchverzeichnis, bei einer Datei ihr Name
	Line  int    `json:"line"`
	Text  string `json:"text"`
	Match bool   `json:"match"`
}

type GrepResult struct {
	IsDir        bool       `json:"isDir"`
	Lines        []GrepLine `json:"lines"`
	Matches      int        `json:"matches"`
	LimitReached bool       `json:"limitReached"`
}

// GlobResult: Zeilen, wie fd sie ausgibt (absolute Pfade, Verzeichnisse mit „/“ am Ende).
type GlobResult struct {
	Paths []string `json:"paths"`
}

// CleanPath prüft einen Pfad aus einer Anfrage: absolut, ohne Steuerzeichen
// und NUL, gültiges UTF-8, nicht zu lang. Er wird bereinigt (path.Clean),
// Symlinks bleiben stehen; die Operation läuft ohnehin als Agent-Nutzer in
// dessen eigener Sandbox.
// Die Meldungen sind englisch: Sie können als Werkzeugergebnis beim Modell ankommen (L4).
func CleanPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	if len(p) > MaxPath {
		return "", errors.New("path too long")
	}
	if !utf8.ValidString(p) {
		return "", errors.New("path is not valid UTF-8")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("control character in path")
		}
	}
	if !strings.HasPrefix(p, "/") {
		return "", errors.New("path must be absolute")
	}
	return path.Clean(p), nil
}

// Validate prüft eine Anfrage vor der Ausführung (beide Seiten).
func (r *Request) Validate() error {
	switch r.Op {
	case OpCancel, OpInput:
		return nil
	case OpWorkflow:
		if len(r.Data) == 0 || len(r.Data) > MaxWorkflowSource {
			return errors.New("workflow source missing or too large")
		}
		return nil
	case OpBash, OpBg:
		if r.Op == OpBg && !BgLogRe.MatchString(r.Spill) {
			return errors.New("invalid background log path")
		}
		if r.Command == "" || len(r.Command) > MaxCommand {
			return errors.New("command missing or too long")
		}
		if strings.ContainsRune(r.Command, 0) {
			return errors.New("NUL in command")
		}
		if math.IsNaN(r.Timeout) || r.Timeout < 0 || r.Timeout > MaxTimeoutSec {
			return fmt.Errorf("Invalid timeout: maximum is %v seconds", MaxTimeoutSec)
		}
		if r.Op == OpBash && r.Spill != "" && !SpillRe.MatchString(r.Spill) {
			return errors.New("invalid spill path")
		}
		c, err := CleanPath(r.Cwd)
		if err != nil {
			return errors.New("working directory: " + err.Error())
		}
		r.Cwd = c
		for k, v := range r.Env {
			if !strings.HasPrefix(k, "PI_") || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) || len(v) > 4096 {
				return errors.New("environment variable not allowed: " + k)
			}
		}
		return nil
	case OpRead, OpWrite, OpMkdir, OpStat, OpReaddir, OpAccess, OpImageType, OpGrep, OpGlob, OpReadLines:
		c, err := CleanPath(r.Path)
		if err != nil {
			return err
		}
		r.Path = c
	default:
		return errors.New("unknown operation " + r.Op)
	}
	switch r.Op {
	case OpWrite:
		if len(r.Data) > MaxFileBytes {
			return errors.New("file too large")
		}
	case OpRead:
		if r.Max <= 0 || r.Max > MaxFileBytes {
			r.Max = MaxFileBytes
		}
	case OpAccess:
		if r.Mode != "r" && r.Mode != "rw" && r.Mode != "" {
			return errors.New("invalid access mode")
		}
	case OpGrep:
		if r.Grep == nil || r.Grep.Pattern == "" || len(r.Grep.Pattern) > 8<<10 {
			return errors.New("search pattern missing or too long")
		}
		if strings.ContainsRune(r.Grep.Pattern, 0) || strings.ContainsRune(r.Grep.Glob, 0) {
			return errors.New("NUL in search pattern")
		}
		if r.Grep.Limit <= 0 {
			r.Grep.Limit = 100
		}
		r.Grep.Limit = min(r.Grep.Limit, MaxGrepLimit)
		r.Grep.Context = max(0, min(r.Grep.Context, 50))
	case OpGlob:
		if r.Glob == nil || r.Glob.Pattern == "" || len(r.Glob.Pattern) > 4096 {
			return errors.New("pattern missing or too long")
		}
		if strings.ContainsRune(r.Glob.Pattern, 0) {
			return errors.New("NUL in pattern")
		}
		if r.Glob.Limit <= 0 {
			r.Glob.Limit = 1000
		}
		r.Glob.Limit = min(r.Glob.Limit, MaxGlobLimit)
	case OpReadLines:
		if r.Lines == nil || r.Lines.Start < 0 || r.Lines.Count <= 0 || r.Lines.MaxBytes <= 0 {
			return errors.New("invalid line window")
		}
		r.Lines.Count = min(r.Lines.Count, 100000)
		r.Lines.MaxBytes = min(r.Lines.MaxBytes, 8<<20)
	}
	return nil
}
