package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"agw/internal/execproto"
)

// The operations themselves. They run in the child process "agw-exec op" as the
// agent user; the supervisor (serve) performs no file operation itself.

type opError struct {
	code string
	msg  string
}

func (e *opError) Error() string { return e.msg }

func errCode(err error) string {
	var oe *opError
	if errors.As(err, &oe) {
		return oe.code
	}
	for _, e := range []struct {
		errno syscall.Errno
		code  string
	}{{syscall.ENOENT, "ENOENT"}, {syscall.EACCES, "EACCES"}, {syscall.EISDIR, "EISDIR"}, {syscall.ENOTDIR, "ENOTDIR"},
		{syscall.EROFS, "EROFS"}, {syscall.ENOSPC, "ENOSPC"}, {syscall.EEXIST, "EEXIST"}, {syscall.ELOOP, "ELOOP"},
		{syscall.ENAMETOOLONG, "ENAMETOOLONG"}, {syscall.EPERM, "EPERM"}} {
		if errors.Is(err, e.errno) {
			return e.code
		}
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "ENOENT"
	case errors.Is(err, fs.ErrPermission):
		return "EACCES"
	}
	return ""
}

func result(v any) execproto.Frame {
	b, _ := json.Marshal(v)
	return execproto.Frame{Done: true, Result: b}
}

func fail(err error) execproto.Frame {
	return execproto.Frame{Done: true, Error: err.Error(), Code: errCode(err)}
}

// runOp executes a validated request and reports frames via emit; the
// last frame has Done set.
func runOp(ctx context.Context, req execproto.Request, emit func(execproto.Frame)) {
	if err := req.Validate(); err != nil {
		emit(fail(&opError{"EINVAL", err.Error()}))
		return
	}
	switch req.Op {
	case execproto.OpRead:
		data, size, err := readFile(req.Path, req.Max)
		if err != nil {
			emit(fail(err))
			return
		}
		emit(result(execproto.ReadResult{Data: data, Size: size}))
	case execproto.OpWrite:
		if err := writeFile(req.Path, req.Data); err != nil {
			emit(fail(err))
			return
		}
		emit(result(struct{}{}))
	case execproto.OpMkdir:
		if err := os.MkdirAll(req.Path, 0o755); err != nil {
			emit(fail(err))
			return
		}
		emit(result(struct{}{}))
	case execproto.OpStat:
		emit(result(statPath(req.Path)))
	case execproto.OpReaddir:
		r, err := readDir(req.Path)
		if err != nil {
			emit(fail(err))
			return
		}
		emit(result(r))
	case execproto.OpAccess:
		mode := uint32(4) // R_OK
		if req.Mode == "rw" {
			mode = 4 | 2
		}
		if err := syscall.Access(req.Path, mode); err != nil {
			emit(fail(&os.PathError{Op: "access", Path: req.Path, Err: err}))
			return
		}
		emit(result(struct{}{}))
	case execproto.OpImageType:
		emit(result(execproto.ImageTypeResult{Mime: imageType(req.Path)}))
	case execproto.OpBash, execproto.OpBg:
		runBash(ctx, req, emit)
	case execproto.OpGrep:
		r, err := grep(ctx, req.Path, *req.Grep)
		if err != nil {
			emit(fail(err))
			return
		}
		emit(result(r))
	case execproto.OpGlob:
		r, err := glob(ctx, req.Path, *req.Glob)
		if err != nil {
			emit(fail(err))
			return
		}
		emit(result(r))
	case execproto.OpReadLines:
		r, err := readLines(ctx, req.Path, *req.Lines)
		if err != nil {
			emit(fail(err))
			return
		}
		emit(result(r))
	default:
		emit(fail(&opError{"EINVAL", "unknown operation"}))
	}
}

// openRegular opens regular files only. O_NONBLOCK prevents a FIFO of the
// agent from holding the operation.
func openRegular(p string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(p, flag|syscall.O_NONBLOCK, perm)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		f.Close()
		return nil, &opError{"EISDIR", "EISDIR: illegal operation on a directory, read"}
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, &opError{"EINVAL", fmt.Sprintf("EINVAL: not a regular file: '%s'", p)}
	}
	return f, nil
}

func readFile(p string, max int64) ([]byte, int64, error) {
	f, err := openRegular(p, os.O_RDONLY, 0)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, _ := f.Stat()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) > max {
		return nil, 0, &opError{"EFBIG", fmt.Sprintf("EFBIG: file too large (over %d MiB): '%s'", max>>20, p)}
	}
	return data, st.Size(), nil
}

func writeFile(p string, data []byte) error {
	f, err := openRegular(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func statPath(p string) execproto.StatResult {
	st, err := os.Stat(p)
	if err != nil {
		return execproto.StatResult{}
	}
	return execproto.StatResult{Exists: true, IsDir: st.IsDir(), IsFile: st.Mode().IsRegular(), Size: st.Size(),
		MtimeMs: st.ModTime().UnixMilli(), Mode: uint32(st.Mode().Perm())}
}

const maxDirEntries = 100000

func readDir(p string) (execproto.ReaddirResult, error) {
	ents, err := os.ReadDir(p)
	if err != nil {
		return execproto.ReaddirResult{}, err
	}
	if len(ents) > maxDirEntries {
		ents = ents[:maxDirEntries]
	}
	out := execproto.ReaddirResult{Entries: make([]execproto.DirEntry, 0, len(ents))}
	for _, e := range ents {
		isDir := e.IsDir()
		if e.Type()&fs.ModeSymlink != 0 {
			st, err := os.Stat(filepath.Join(p, e.Name()))
			if err != nil {
				// Like pi (ls stats every entry and skips what cannot be
				// read): broken symlinks do not appear (L3).
				continue
			}
			isDir = st.IsDir()
		}
		out.Entries = append(out.Entries, execproto.DirEntry{Name: e.Name(), IsDir: isDir})
	}
	return out, nil
}

// imageType recognises PNG, JPEG, GIF and WebP by their first bytes, like pi.
func imageType(p string) string {
	f, err := openRegular(p, os.O_RDONLY, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	b := make([]byte, 16)
	n, _ := io.ReadFull(f, b)
	return sniffImage(b[:n])
}

func sniffImage(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "image/gif"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}

// --- bash ---

// spill additionally writes a command's output into a file in the execution
// sandbox (as the agent user), so that for long output pi can refer to "Full output: <path>"
// as usual and a read on it finds the file (H1). Previously pi wrote this file in its
// own container (tmpfs 256 MiB); with a full tmpfs pi died of an unhandled error.
// Upper limit MaxSpillBytes with a note. Below pi's thresholds the file is deleted again.
type spill struct {
	path      string
	f         *os.File
	written   int64
	raw       int64
	newlines  int64
	last      byte
	decoded   int64  // bytes after UTF-8 decoding with replacement characters (upper estimate)
	pend      []byte // incomplete UTF-8 sequence at the end of the last chunk
	truncated bool
	err       error
	// always: always keep the file (background task), limit: upper limit of the file.
	always bool
	limit  int64
}

// maxSpill is execproto.MaxSpillBytes; smaller in tests.
var maxSpill int64 = execproto.MaxSpillBytes

func maxSpillForTest(n int64) int64 { old := maxSpill; maxSpill = n; return old }

// openSpill creates the file for the full output (before the command starts). f: already open
// file (background task, created by the supervisor) or nil.
func openSpill(p string, f *os.File) *spill {
	s := &spill{path: p, limit: maxSpill}
	if f != nil {
		s.f = f
		return s
	}
	if p == "" {
		return s
	}
	f, err := openSpillFile(p)
	if err != nil {
		s.err = err
		return s
	}
	s.f = f
	return s
}

// openSpillFile opens an output file for writing without letting itself be held up (Review 3,
// N1): O_NONBLOCK, so that a FIFO of the agent at this path does not wait for a reader
// (without a reader: ENXIO), O_NOFOLLOW against symlinks, then regular files only; O_NONBLOCK is
// cleared again for writing.
func openSpillFile(p string) (*os.File, error) {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o644)
	if err != nil {
		return nil, err
	}
	if err := regularBlocking(f, p); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// regularBlocking checks that f is a regular file and switches O_NONBLOCK off.
func regularBlocking(f *os.File, p string) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return &opError{"EINVAL", fmt.Sprintf("EINVAL: not a regular file: '%s'", p)}
	}
	return syscall.SetNonblock(int(f.Fd()), false)
}

func (s *spill) write(b []byte) {
	s.raw += int64(len(b))
	s.newlines += int64(bytes.Count(b, []byte{'\n'}))
	if len(b) > 0 {
		s.last = b[len(b)-1]
	}
	s.countDecoded(b)
	if s.f == nil || s.err != nil || s.truncated {
		return
	}
	room := s.limit - s.written
	if int64(len(b)) > room {
		b = b[:room]
		s.truncated = true
	}
	n, err := s.f.Write(b)
	s.written += int64(n)
	if err != nil {
		s.err = err
	}
}

// countDecoded estimates the length after TextDecoder (invalid bytes as U+FFFD, three bytes). The
// estimate is never below; so the file is kept once too often rather than too rarely.
func (s *spill) countDecoded(b []byte) {
	data := append(s.pend, b...)
	s.pend = nil
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			if !utf8.FullRune(data[i:]) && len(data)-i < utf8.UTFMax {
				s.pend = append([]byte(nil), data[i:]...)
				return
			}
			s.decoded += 3
			i++
			continue
		}
		s.decoded += int64(size)
		i += size
	}
}

// finish decides about the file and returns its path if it stays.
func (s *spill) finish() (string, string) {
	if len(s.pend) > 0 {
		s.decoded += 3
		s.pend = nil
	}
	lines := s.newlines
	if s.raw > 0 && s.last != '\n' {
		lines++
	}
	keep := s.always || s.raw > execproto.PiMaxBytes || s.decoded > execproto.PiMaxBytes || lines > execproto.PiMaxLines
	if s.f == nil {
		if keep && s.err != nil {
			return "", s.err.Error()
		}
		return "", ""
	}
	if s.truncated && s.err == nil {
		_, s.err = fmt.Fprintf(s.f, "\n\n[output truncated after %d MiB of %d bytes]\n", s.limit>>20, s.raw)
		if s.err != nil {
			s.err = nil // the note is dispensable
		}
	}
	cerr := s.f.Close()
	if !keep {
		_ = os.Remove(s.path)
		return "", ""
	}
	if s.err == nil {
		s.err = cerr
	}
	if s.err != nil {
		return s.path, s.err.Error()
	}
	return s.path, ""
}

// runBash runs the command with bash -c in its own process group
// and streams stdout and stderr together. Abort and timeout kill the
// whole group. As with pi, a background process that keeps the output
// open keeps running; after the shell ends, reading continues only briefly.
//
// A background task (OpBg) runs the same way, except that it always keeps its output file
// (/tmp/agw-bg/bg-<n>.log, at most MaxBgLogBytes) and first reports its
// process group. It runs until the command ends or the orchestrator aborts.
func runBash(ctx context.Context, req execproto.Request, emit func(execproto.Frame)) {
	if st, err := os.Stat(req.Cwd); err != nil || !st.IsDir() {
		emit(fail(&opError{"ENOENT", "Working directory does not exist: " + req.Cwd + "\nCannot execute bash commands."}))
		return
	}
	bg := req.Op == execproto.OpBg
	logPath := req.Spill
	var logFile *os.File
	if bg {
		logPath = bgLogFile(req.Spill)
		if req.LogFD {
			// Created by the supervisor and handed over open (descriptor 3).
			logFile = os.NewFile(3, logPath)
			if err := regularBlocking(logFile, logPath); err != nil {
				logFile.Close()
				logFile = nil
			}
		} else if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
			emit(fail(err))
			return
		}
	} else if spillDir != "" && logPath != "" {
		logPath = filepath.Join(spillDir, filepath.Base(logPath))
	}
	// Open the file before the start: if opening hangs, no command should be running yet (N1).
	sp := openSpill(logPath, logFile)
	pr, pw, err := os.Pipe()
	if err != nil {
		sp.finish()
		emit(fail(err))
		return
	}
	cmd := exec.Command(bashPath, "-c", req.Command)
	cmd.Dir = req.Cwd
	cmd.Env = commandEnv(req.Env)
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = pw, pw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		sp.finish()
		emit(fail(err))
		return
	}
	pw.Close()
	pgid := cmd.Process.Pid
	if bg {
		sp.always, sp.limit = true, execproto.MaxBgLogBytes
		if sp.err != nil {
			// Without a file the task keeps running; the orchestrator has the output anyway.
			fmt.Fprintln(os.Stderr, "agw-exec: output file of the background task:", sp.err)
		}
		emit(execproto.Frame{Pgid: pgid})
	}
	var outMu sync.Mutex
	readDone := make(chan struct{})
	var thr throttle
	if bg {
		thr = throttle{after: bgThrottleAfter, rate: bgThrottleRate}
	}
	go func() {
		defer close(readDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				outMu.Lock()
				sp.write(buf[:n])
				emit(execproto.Frame{Data: append([]byte(nil), buf[:n]...)})
				outMu.Unlock()
				thr.wait(n)
			}
			if err != nil {
				return
			}
		}
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	var timer <-chan time.Time
	if req.Timeout > 0 {
		t := time.NewTimer(time.Duration(req.Timeout * float64(time.Second)))
		defer t.Stop()
		timer = t.C
	}
	reason := ""
	var werr error
	select {
	case werr = <-waitDone:
	case <-ctx.Done():
		reason = "aborted"
	case <-timer:
		reason = "timeout"
	}
	if reason != "" {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		werr = <-waitDone
	}
	// Keep reading briefly after the shell ends; stragglers in the background
	// would otherwise keep the output open indefinitely.
	select {
	case <-readDone:
	case <-time.After(150 * time.Millisecond):
	}
	pr.Close()
	<-readDone
	outMu.Lock()
	defer outMu.Unlock()
	full, spillErr := sp.finish()
	if reason != "" {
		if reason == "aborted" {
			// On abort also kill stragglers of the group.
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		emit(execproto.Frame{Done: true, Error: reason, Code: reason, FullOutputPath: full, SpillError: spillErr})
		return
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		} else {
			code = ee.ExitCode()
		}
	} else if werr != nil {
		emit(fail(werr))
		return
	}
	emit(execproto.Frame{Done: true, Exit: &code, FullOutputPath: full, SpillError: spillErr})
}

var bashPath = "/bin/bash"

// throttle limits reading to a rate after a certain amount of output (N3): the command then waits
// when writing into the pipe, and the orchestrator receives at most rate bytes per second.
type throttle struct {
	after, rate int64
	seen, over  int64
	start       time.Time
}

func (t *throttle) wait(n int) {
	if t.rate <= 0 {
		return
	}
	t.seen += int64(n)
	if t.seen <= t.after {
		return
	}
	if t.start.IsZero() {
		t.start, t.over = time.Now(), t.seen-t.after
		return
	}
	t.over += int64(n)
	want := time.Duration(float64(t.over) / float64(t.rate) * float64(time.Second))
	if d := want - time.Since(t.start); d > 0 {
		time.Sleep(d)
	}
}

// Throttling of background tasks (tests set smaller values).
var (
	bgThrottleAfter int64 = execproto.BgThrottleAfter
	bgThrottleRate  int64 = execproto.BgThrottleRate
)

// spillDir redirects /tmp/pi-bash-*.log into a separate directory in tests.
var spillDir = ""

func spillDirForTest(d string) string { old := spillDir; spillDir = d; return old }

// bgLogFile forms the path of a background task's output file. Tests redirect it with
// AGW_EXEC_BG_DIR into a separate directory (the variable does not reach the agent's commands,
// commandEnv removes AGW_EXEC_*; only whoever starts the supervisor can set it).
func bgLogFile(p string) string {
	if d := os.Getenv("AGW_EXEC_BG_DIR"); d != "" {
		return filepath.Join(d, filepath.Base(p))
	}
	return p
}

// baseEnv is the sandbox's environment (passed on by the supervisor); added to it
// are HOME and user of the agent and the session's PI_* values.
func commandEnv(extra map[string]string) []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	for k := range env {
		if strings.HasPrefix(k, "AGW_EXEC_") {
			delete(env, k)
		}
	}
	for k, v := range extra {
		if strings.HasPrefix(k, "PI_") {
			env[k] = v
		}
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// --- grep and glob via ripgrep ---

var rgPath = "rg"

// rgData is a field of rg --json: text as "text" or, with invalid UTF-8, as "bytes"
// (base64). pi reads only "text" and leaves such matches without output; here the bytes
// are decoded (L5). The path stays usable as a byte sequence for reading context lines.
type rgData struct {
	Text  *string `json:"text"`
	Bytes *string `json:"bytes"`
}

func (d rgData) String() string {
	if d.Text != nil {
		return *d.Text
	}
	if d.Bytes != nil {
		if b, err := base64.StdEncoding.DecodeString(*d.Bytes); err == nil {
			return string(b)
		}
	}
	return ""
}

// grep mirrors pi's grep: rg --json, at most Limit matches, context from
// the file, paths relative to the search directory.
func grep(ctx context.Context, searchPath string, a execproto.GrepArgs) (execproto.GrepResult, error) {
	st, err := os.Stat(searchPath)
	if err != nil {
		return execproto.GrepResult{}, &opError{"ENOENT", "Path not found: " + searchPath}
	}
	res := execproto.GrepResult{IsDir: st.IsDir(), Lines: []execproto.GrepLine{}}
	args := []string{"--json", "--line-number", "--color=never", "--hidden"}
	if a.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	if a.Literal {
		args = append(args, "--fixed-strings")
	}
	if a.Glob != "" {
		args = append(args, "--glob", a.Glob)
	}
	args = append(args, "--", a.Pattern, searchPath)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, rgPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("ripgrep: %w", err)
	}
	type match struct {
		path string
		line int
		text string
	}
	var matches []match
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Path  rgData `json:"path"`
				Lines rgData `json:"lines"`
				Line  int    `json:"line_number"`
			} `json:"data"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Type != "match" {
			continue
		}
		matches = append(matches, match{ev.Data.Path.String(), ev.Data.Line, ev.Data.Lines.String()})
		if len(matches) >= a.Limit {
			res.LimitReached = true
			cancel()
			break
		}
	}
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return res, &opError{"aborted", "Operation aborted"}
	}
	if !res.LimitReached && werr != nil {
		var ee *exec.ExitError
		if !errors.As(werr, &ee) || ee.ExitCode() != 1 {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = "ripgrep: " + werr.Error()
			}
			return res, errors.New(msg)
		}
	}
	res.Matches = len(matches)
	display := func(p string) string {
		if res.IsDir {
			if rel, err := filepath.Rel(searchPath, p); err == nil && rel != "" && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel)
			}
		}
		return filepath.Base(p)
	}
	cache := map[string][]string{}
	lines := func(p string) []string {
		if l, ok := cache[p]; ok {
			return l
		}
		data, _, err := readFile(p, execproto.MaxFileBytes)
		var l []string
		if err == nil {
			s := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
			l = strings.Split(s, "\n")
		}
		cache[p] = l
		return l
	}
	for _, m := range matches {
		dp := strings.ToValidUTF8(display(m.path), "\uFFFD")
		if a.Context == 0 {
			t := strings.TrimSuffix(strings.ReplaceAll(strings.ReplaceAll(strings.ToValidUTF8(m.text, "\uFFFD"), "\r\n", "\n"), "\r", ""), "\n")
			res.Lines = append(res.Lines, execproto.GrepLine{Path: dp, Line: m.line, Text: t, Match: true})
			continue
		}
		l := lines(m.path)
		if len(l) == 0 {
			res.Lines = append(res.Lines, execproto.GrepLine{Path: dp, Line: m.line, Text: "(unable to read file)", Match: true})
			continue
		}
		from, to := max(1, m.line-a.Context), min(len(l), m.line+a.Context)
		for i := from; i <= to; i++ {
			res.Lines = append(res.Lines, execproto.GrepLine{Path: dp, Line: i, Text: strings.ToValidUTF8(strings.ReplaceAll(l[i-1], "\r", ""), "\uFFFD"), Match: i == m.line})
		}
	}
	return res, nil
}

var fdPath = "fd"

// glob is pi's find with fd, with the same arguments as pi (dist/core/tools/find.js): glob against
// the name, for patterns with "/" against the whole path with a leading "**/"; hidden
// files included, .gitignore respected (outside a Git repository with --no-require-git);
// directories appear with "/" at the end. If fd ends with an error but with output, the output
// counts, as with pi (M2).
func glob(ctx context.Context, searchPath string, a execproto.GlobArgs) (execproto.GlobResult, error) {
	args := []string{"--glob", "--color=never", "--hidden"}
	if !insideGitRepo(searchPath) {
		args = append(args, "--no-require-git")
	}
	args = append(args, "--max-results", strconv.Itoa(a.Limit))
	pattern := a.Pattern
	if strings.Contains(pattern, "/") {
		args = append(args, "--full-path")
		if !strings.HasPrefix(pattern, "/") && !strings.HasPrefix(pattern, "**/") && pattern != "**" {
			pattern = "**/" + pattern
		}
	}
	args = append(args, "--", pattern, searchPath)
	cmd := exec.CommandContext(ctx, fdPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedBuffer{b: &stdout, max: 64 << 20}
	cmd.Stderr = &limitedBuffer{b: &stderr, max: 64 << 10}
	err := cmd.Run()
	if ctx.Err() != nil {
		return execproto.GlobResult{}, &opError{"aborted", "Operation aborted"}
	}
	res := execproto.GlobResult{Paths: []string{}}
	for _, l := range strings.Split(stdout.String(), "\n") {
		if l != "" {
			res.Paths = append(res.Paths, l)
		}
	}
	if err != nil && len(res.Paths) == 0 {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return res, fmt.Errorf("Failed to run fd: %w", err)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = fmt.Sprintf("fd exited with code %d", ee.ExitCode())
		}
		return res, errors.New(msg)
	}
	return res, nil
}

// insideGitRepo like pi: is there a .git in searchPath or above it?
func insideGitRepo(p string) bool {
	for cur := p; ; {
		if _, err := os.Lstat(filepath.Join(cur, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

type limitedBuffer struct {
	b   *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.b.Len(); room > 0 {
		l.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// readLines returns an excerpt of a text file without loading it fully into memory:
// read beyond MaxFileBytes (e.g. the full output of a command, up to 256 MiB). pi would read the file
// fully; the bridge forms the same output as pi's read from the excerpt (L4).
func readLines(ctx context.Context, p string, a execproto.LinesArgs) (execproto.LinesResult, error) {
	f, err := openRegular(p, os.O_RDONLY, 0)
	if err != nil {
		return execproto.LinesResult{}, err
	}
	defer f.Close()
	res := execproto.LinesResult{Lines: [][]byte{}}
	br := bufio.NewReaderSize(f, 256<<10)
	var line int64
	var cur []byte // current line in the window (truncated to MaxBytes+1)
	var curLen int64
	collected := 0
	full := false
	inWindow := func() bool { return line >= a.Start && line < a.Start+int64(a.Count) && !full }
	selected := func() bool { return line >= a.Start && (a.Select == 0 || line < a.Start+a.Select) }
	endLine := func() {
		if line == a.Start {
			res.StartLineBytes = curLen
		}
		if selected() {
			if res.SelLines > 0 {
				res.SelBytes++ // line break before it
			}
			res.SelBytes += curLen
			res.SelLines++
			res.SelLastEmpty = curLen == 0
		}
		if inWindow() {
			res.Lines = append(res.Lines, cur)
			collected += len(cur) + 1
			if collected > a.MaxBytes {
				full = true
			}
		}
		cur, curLen = nil, 0
		line++
	}
	for n := 0; ; n++ {
		if n%1024 == 0 && ctx.Err() != nil {
			return res, &opError{"aborted", "Operation aborted"}
		}
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			body := chunk
			nl := body[len(body)-1] == '\n'
			if nl {
				body = body[:len(body)-1]
			}
			curLen += int64(len(body))
			if inWindow() && len(cur) <= a.MaxBytes {
				cur = append(cur, body[:min(len(body), a.MaxBytes+1-len(cur))]...)
			}
			if nl {
				endLine()
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			endLine() // like split("\n"): after the last "\n" another (empty) line follows
			break
		}
		if err != nil {
			return res, err
		}
	}
	res.TotalLines = line
	return res, nil
}
