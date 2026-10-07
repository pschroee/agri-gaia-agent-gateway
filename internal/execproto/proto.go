// Package execproto is the protocol between the orchestrator and the helper
// agw-exec in the execution sandbox (E9). Both sides exchange JSON lines:
// the orchestrator sends requests, agw-exec answers with frames carrying the same
// ID. A command (bash) delivers any number of frames with data and finally
// one frame with Done; all other operations only the last one.
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

// Operations. Each runs in the execution sandbox as the agent user.
const (
	OpRead      = "read"       // read a file (regular files only, at most Max bytes)
	OpWrite     = "write"      // write a file (Data)
	OpMkdir     = "mkdir"      // create a directory including parents
	OpStat      = "stat"       // information about a path (follows symlinks)
	OpReaddir   = "readdir"    // entries of a directory including type
	OpAccess    = "access"     // readable (Mode "r") or readable and writable ("rw")
	OpImageType = "image_type" // image type from the magic bytes
	OpBash      = "bash"       // command with bash -c, output streamed
	OpBg        = "bg"         // background task: like bash, output additionally always in /tmp/agw-bg/bg-<n>.log
	OpGrep      = "grep"       // search with ripgrep
	OpGlob      = "glob"       // files and directories by pattern (fd, like pi's find)
	OpReadLines = "read_lines" // window of a large text file by lines (read above MaxFileBytes)
	OpWorkflow  = "workflow"   // script of a workflow (pi-subagents) in its own Node process
	OpInput     = "input"      // further input to a running operation (workflow only)
	OpCancel    = "cancel"     // abort the running operation with the same ID
)

// Ops are all operations except cancel.
var Ops = []string{OpRead, OpWrite, OpMkdir, OpStat, OpReaddir, OpAccess, OpImageType, OpBash, OpBg, OpGrep, OpGlob, OpReadLines, OpWorkflow}

// Limits enforced by both sides.
const (
	MaxFileBytes  = 64 << 20 // read and write (artifacts up to AGW_ARTIFACT_MAX_MB, default 50)
	MaxPath       = 4096
	MaxCommand    = 256 << 10
	MaxFrameBytes = 96 << 20 // one JSON line (base64 of a file at the limit fits)
	MaxGrepLimit  = 1000
	MaxGlobLimit  = 100000
	// MaxTimeoutSec like pi: setTimeout takes at most 2^31-1 ms (L1).
	MaxTimeoutSec = 2147483.647
	// MaxSpillBytes: at most this much of a command's output ends up in the file with the whole
	// output (/tmp/pi-bash-*.log in the execution sandbox, H1).
	MaxSpillBytes = 256 << 20
	// pi's thresholds (DEFAULT_MAX_BYTES, DEFAULT_MAX_LINES): below them no file.
	PiMaxBytes        = 50 << 10
	PiMaxLines        = 2000
	MaxWorkflowSource = 4 << 20
	// MaxBgLogBytes: at most this much of a background task's output ends up in its file.
	MaxBgLogBytes = 256 << 20
	// DefaultBgMax: at most this many background tasks run concurrently per slot (AGW_BG_MAX).
	DefaultBgMax = 5
	// BgThrottleAfter, BgThrottleRate: after this much output agw-exec reads a background task
	// only at this rate (bytes per second); the command then waits when writing. This keeps
	// the load in the orchestrator (checksum, reading along) bounded (Review 3, N3).
	BgThrottleAfter = 64 << 20
	BgThrottleRate  = 4 << 20
)

// BgLogRe: the only permitted path for the output of a background task. The orchestrator
// builds it from the task's sequence number within the chat (BgLogPath).
var BgLogRe = regexp.MustCompile(`^/tmp/agw-bg/bg-[1-9][0-9]{0,8}\.log$`)

// BgLogDir holds the outputs of the background tasks in the execution sandbox.
const BgLogDir = "/tmp/agw-bg"

// BgLogPath: /tmp/agw-bg/bg-<n>.log
func BgLogPath(seq int) string { return fmt.Sprintf("%s/bg-%d.log", BgLogDir, seq) }

// SpillRe: the only permitted path for the whole output of a command. The orchestrator
// builds it from the toolCallId (SpillPath), the agent cannot choose it.
var SpillRe = regexp.MustCompile(`^/tmp/pi-bash-[0-9a-f]{16}\.log$`)

// SpillPath: /tmp/pi-bash-<first 16 hex characters of sha256(toolCallId)>.log
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
	Timeout float64           `json:"timeout,omitempty"` // seconds, 0 = none
	Grep    *GrepArgs         `json:"grep,omitempty"`
	Glob    *GlobArgs         `json:"glob,omitempty"`
	Lines   *LinesArgs        `json:"lines,omitempty"`
	// Spill: file for the whole output of bash (SpillRe) or of a background task (BgLogRe);
	// set by the orchestrator.
	Spill string `json:"spill,omitempty"`
	// LogFD is set only by the supervisor for its helper: the output file of a background task
	// is already open (descriptor 3), created by the supervisor as root (Review 3, N1). Whatever the
	// orchestrator sends here is overwritten by the supervisor.
	LogFD bool `json:"logFd,omitempty"`
	// Root (only read): the file must lie inside this directory, no symbolic link on the way or at the end, regular
	// file only (OpenInside). Set by the orchestrator for files the agent sends to the user (issue #62).
	Root string `json:"root,omitempty"`
}

// LinesArgs: lines Start (0-based) to Start+Count-1, at most a little more than MaxBytes.
type LinesArgs struct {
	Start    int64 `json:"start"`
	Count    int   `json:"count"`
	MaxBytes int   `json:"maxBytes"`
	// Select: this many lines from Start the caller has selected (0: to the end). For them
	// readLines returns size and line count, as pi's truncateHead counts them over the selection.
	Select int64 `json:"select,omitempty"`
}

// LinesResult: lines as bytes (without "\n"), total count like JavaScript's split("\n") and the
// length of the first requested line, even if Lines holds it only truncated.
type LinesResult struct {
	Lines          [][]byte `json:"lines"`
	TotalLines     int64    `json:"totalLines"`
	StartLineBytes int64    `json:"startLineBytes"`
	// Selection (Select): bytes including line breaks, lines, last line empty.
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

// Frame is a response line. Data carries output of a command, Done ends
// the operation (with Exit for bash, Result for the others, or Error).
type Frame struct {
	ID     uint64          `json:"id"`
	Data   []byte          `json:"data,omitempty"`
	Done   bool            `json:"done,omitempty"`
	Exit   *int            `json:"exit,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"` // ENOENT, EISDIR, EACCES, timeout, aborted, …
	Result json.RawMessage `json:"result,omitempty"`
	// bash only: file with the whole output (above pi's thresholds), in the execution sandbox.
	FullOutputPath string `json:"fullOutputPath,omitempty"`
	// bg only: process group of the command, reported in the first frame. If the helper ends without
	// a result (e.g. because the agent killed it), the supervisor kills this group.
	Pgid       int    `json:"pgid,omitempty"`
	SpillError string `json:"spillError,omitempty"`
	// Only bash at pi's socket, code "backgrounded": the user has turned the command into a
	// background task; its ID (bg-<n>).
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

// GrepLine is a line of grep's output: match or context.
type GrepLine struct {
	Path  string `json:"path"` // like pi: relative to the search directory, for a file its name
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

// GlobResult: lines as fd prints them (absolute paths, directories with "/" at the end).
type GlobResult struct {
	Paths []string `json:"paths"`
}

// CleanPath checks a path from a request: absolute, without control characters
// and NUL, valid UTF-8, not too long. It is cleaned (path.Clean),
// symlinks are left alone; the operation runs as the agent user in
// its own sandbox anyway.
// The messages are in English: they can reach the model as a tool result (L4).
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

// Validate checks a request before execution (both sides).
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
	if r.Root != "" && r.Op != OpRead {
		return errors.New("root only for read")
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
		if r.Root != "" {
			c, err := CleanPath(r.Root)
			if err != nil {
				return errors.New("root: " + err.Error())
			}
			r.Root = c
			if !Inside(r.Root, r.Path) {
				return fmt.Errorf("only files inside %s can be sent, not %s: %w", r.Root, r.Path, ErrOutsideRoot)
			}
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
