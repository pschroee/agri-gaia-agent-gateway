package chat

// Workspace per chat: /workspace survives idling. After every completed run
// and when idling, the orchestrator packs /workspace in the sandbox (without
// inputs/ and without package and cache folders) as tar.gz and stores it in S3
// under <chat>/workspace.tar.gz; on resuming it restores it into the fresh
// sandbox before the first message arrives.
//
// The archive comes from the sandbox of the same chat and can therefore be
// shaped by the agent. Before restoring, the orchestrator therefore reads it
// itself and passes on only harmless entries (regular files, folders,
// symlinks, hard links within the archive; no absolute paths, no "..", nothing
// under a symlink, nothing under inputs/). It is unpacked as the agent user
// without the owners from the archive.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"agw/internal/store"
)

// DefaultWorkspaceMaxBytes applies when Options.WorkspaceMaxBytes is 0.
const DefaultWorkspaceMaxBytes = 200 << 20

// workspaceTimeout limits packing and unpacking.
const workspaceTimeout = 2 * time.Minute

// WorkspaceExcludes are folder names that are never backed up, at whatever
// depth: packages installed later and caches (the user's decision: packages
// are not kept). inputs/ at the top level comes from the chat's inputs anyway.
var WorkspaceExcludes = []string{"node_modules", ".venv", "__pycache__", ".cache"}

func workspaceObjectKey(chatID string) string { return path.Join(chatID, "workspace.tar.gz") }

// workspaceSaveScript (bash, as the agent user) determines fingerprint, file
// count and size of /workspace and packs it if it has changed since the last
// backup and is below the limit. First line of the output:
//
//	SAME <fp> <files> <bytes>   unchanged
//	SKIP <fp> <files> <bytes>   above the limit, nothing packed
//	DATA <fp> <files> <bytes>   followed by the tar.gz
//
// The fingerprint counts only files, folders and symlinks (anything else is
// dropped on restore), for folders without size and time (those also change
// through excluded subfolders such as __pycache__); --format=posix keeps the times to the nanosecond,
// so that the state after restoring has the same fingerprint again.
// head -c limits the output hard (if a file grows while packing).
// tar status 1 (file changed while being read) counts as success.
var workspaceSaveScript = `# agw:workspace-save
set -o pipefail
max=$1; last=$2; cap=$3
cd /workspace || exit 3
list() { find . \( -path ./inputs` + findExcludes() + ` \) -prune -o ! -path . \( -type d -printf 'd\t0\t0\t%m\t%P\t\n' -o \( -type f -o -type l \) -printf '%y\t%s\t%T@\t%m\t%P\t%l\n' \); }
fp=$(list | LC_ALL=C sort | sha256sum | cut -c1-64)
read -r files size < <(list | awk -F'\t' '$1=="f"{n++; s+=$2} END{printf "%d %.0f\n", n, s}')
if [ "$fp" = "$last" ]; then echo "SAME $fp $files $size"; exit 0; fi
if [ "$size" -gt "$max" ]; then echo "SKIP $fp $files $size"; exit 0; fi
echo "DATA $fp $files $size"
( tar -C /workspace --exclude=./inputs` + tarExcludes() + ` --format=posix --warning=no-file-changed --warning=no-file-removed -czf - . ; rc=$?; [ "$rc" -le 1 ] ) | head -c "$cap"`

// workspaceRestoreScript unpacks a tar stream (filtered by the orchestrator)
// into /workspace, without the owners from the archive.
const workspaceRestoreScript = `# agw:workspace-restore
exec tar -C /workspace -xf - --no-same-owner --delay-directory-restore`

func findExcludes() string {
	var b strings.Builder
	for _, x := range WorkspaceExcludes {
		b.WriteString(" -o -name " + x)
	}
	return b.String()
}

func tarExcludes() string {
	var b strings.Builder
	for _, x := range WorkspaceExcludes {
		b.WriteString(" --exclude=" + x)
	}
	return b.String()
}

type execer interface {
	Exec(ctx context.Context, cmd []string, stdin io.Reader) ([]byte, error)
}

// wsSnapshot is the result of the backup script.
type wsSnapshot struct {
	Status      string // SAME, SKIP, DATA
	Fingerprint string
	Files       int
	Size        int64
	Archive     []byte // only for DATA
}

// workspaceArchiveCap is the hard upper bound for the archive (tar headers,
// padding to 512 bytes per file, growth while packing).
func workspaceArchiveCap(limit int64) int64 { return 2*limit + 16<<20 }

// snapshotWorkspace runs the backup script in the sandbox.
func snapshotWorkspace(ctx context.Context, a execer, limit int64, lastFP string) (wsSnapshot, error) {
	if lastFP == "" {
		lastFP = "-"
	}
	capBytes := workspaceArchiveCap(limit)
	out, err := a.Exec(ctx, []string{"bash", "-c", workspaceSaveScript, "bash",
		strconv.FormatInt(limit, 10), lastFP, strconv.FormatInt(capBytes+1, 10)}, nil)
	head, rest, ok := bytes.Cut(out, []byte("\n"))
	var s wsSnapshot
	f := strings.Fields(string(head))
	if !ok || len(f) != 4 {
		if err != nil {
			return s, err
		}
		return s, fmt.Errorf("unexpected output of the backup script: %q", truncate(string(head), 200))
	}
	s.Status, s.Fingerprint = f[0], f[1]
	s.Files, _ = strconv.Atoi(f[2])
	s.Size, _ = strconv.ParseInt(f[3], 10, 64)
	switch s.Status {
	case "SAME", "SKIP":
		return s, err
	case "DATA":
	default:
		return s, fmt.Errorf("unexpected status %q", s.Status)
	}
	if int64(len(rest)) > capBytes {
		// Grew while packing: treat as above the limit.
		s.Status = "SKIP"
		s.Size = max(s.Size, int64(len(rest)))
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.Archive = rest
	return s, nil
}

// filterWorkspaceArchive reads a tar.gz and writes the harmless entries as an
// uncompressed tar to w. Returns the number of kept and of dropped entries.
// maxContent limits the sum of the file sizes (protection against archives
// that become oversized when unpacked).
func filterWorkspaceArchive(r io.Reader, w io.Writer, maxContent int64) (kept, dropped int, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, 0, fmt.Errorf("not gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	tw := tar.NewWriter(w)
	symlinks := map[string]bool{}
	regular := map[string]bool{}
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return kept, dropped, fmt.Errorf("archive corrupt: %w", err)
		}
		name, ok := safeArchivePath(h.Name)
		if !ok || underSymlink(name, symlinks) {
			dropped++
			continue
		}
		if name == "." {
			continue // root: /workspace already exists
		}
		out := &tar.Header{Name: name, Mode: h.Mode & 0o7777 &^ 0o6000, ModTime: h.ModTime, Format: tar.FormatPAX}
		switch h.Typeflag {
		case tar.TypeDir:
			out.Typeflag = tar.TypeDir
			out.Name += "/"
		case tar.TypeReg, tar.TypeRegA:
			total += h.Size
			if total > maxContent {
				return kept, dropped, fmt.Errorf("content larger than %d bytes", maxContent)
			}
			out.Typeflag, out.Size = tar.TypeReg, h.Size
		case tar.TypeSymlink:
			if h.Linkname == "" || strings.ContainsRune(h.Linkname, 0) {
				dropped++
				continue
			}
			out.Typeflag, out.Linkname = tar.TypeSymlink, h.Linkname
		case tar.TypeLink:
			target, ok := safeArchivePath(h.Linkname)
			if !ok || !regular[target] {
				dropped++
				continue
			}
			out.Typeflag, out.Linkname = tar.TypeLink, target
		default: // FIFOs, devices and everything else
			dropped++
			continue
		}
		// A name that already occurred as another type is not replaced.
		if symlinks[name] || (regular[name] && out.Typeflag != tar.TypeReg) {
			dropped++
			continue
		}
		if err := tw.WriteHeader(out); err != nil {
			return kept, dropped, err
		}
		if out.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, tr, h.Size); err != nil {
				return kept, dropped, fmt.Errorf("archive corrupt: %w", err)
			}
			regular[name] = true
		}
		if out.Typeflag == tar.TypeSymlink {
			symlinks[name] = true
		}
		kept++
	}
	return kept, dropped, tw.Close()
}

// safeArchivePath cleans a name from the archive. Refused: absolute paths,
// "..", control characters and everything under inputs/ (comes from the inputs).
func safeArchivePath(name string) (string, bool) {
	if name == "" || strings.HasPrefix(name, "/") || len(name) > 4096 {
		return "", false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", false
		}
	}
	p := path.Clean(name)
	if p == "inputs" || strings.HasPrefix(p, "inputs/") {
		return "", false
	}
	return p, true
}

// underSymlink: does the path lie under a symlink from the same archive? Then
// tar would follow the symlink when unpacking.
func underSymlink(p string, symlinks map[string]bool) bool {
	for i := 0; i < len(p); i++ {
		if p[i] == '/' && symlinks[p[:i]] {
			return true
		}
	}
	return false
}

// restoreWorkspaceArchive filters the archive and unpacks it in the sandbox.
func restoreWorkspaceArchive(ctx context.Context, a execer, archive []byte, maxContent int64) (kept, dropped int, err error) {
	pr, pw := io.Pipe()
	var ferr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		kept, dropped, ferr = filterWorkspaceArchive(bytes.NewReader(archive), pw, maxContent)
		pw.CloseWithError(ferr)
	}()
	_, err = a.Exec(ctx, []string{"sh", "-c", workspaceRestoreScript}, pr)
	pr.CloseWithError(errors.New("aborted"))
	wg.Wait()
	if ferr != nil {
		return kept, dropped, ferr
	}
	return kept, dropped, err
}

func (m *Manager) workspaceMax() int64 {
	if m.opt.WorkspaceMaxBytes > 0 {
		return m.opt.WorkspaceMaxBytes
	}
	return DefaultWorkspaceMaxBytes
}

func (m *Manager) workspaceEnabled() bool { return m.opt.WorkspaceMaxBytes >= 0 }

// workspaceLock serialises backing up and restoring per chat.
func (m *Manager) workspaceLock(chatID string) func() {
	m.mu.Lock()
	l, ok := m.wsMu[chatID]
	if !ok {
		l = &sync.Mutex{}
		m.wsMu[chatID] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// saveWorkspace backs up /workspace of the chat's sandbox if it has changed.
// Above the limit nothing is backed up; the last valid backup stays, and the
// chat gets a notice. The reason and the notice are matched by the web UI and
// the CLI (web/src/hooks/useChatStream.ts) and stay German for now.
func (m *Manager) saveWorkspace(ctx context.Context, chatID string, l *live) error {
	if !m.workspaceEnabled() {
		return nil
	}
	unlock := m.workspaceLock(chatID)
	defer unlock()
	select {
	case <-l.stop:
		return nil // sandbox already torn down
	default:
	}
	m.mu.Lock()
	noSave := l.wsNoSave
	m.mu.Unlock()
	if noSave {
		return nil
	}
	ctx, cancel := workspaceContext(ctx)
	defer cancel()
	prev, err := m.st.GetWorkspace(ctx, chatID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	limit := m.workspaceMax()
	snap, err := snapshotWorkspace(ctx, l.slot.Worker, limit, prev.Fingerprint)
	if err != nil {
		return err
	}
	switch snap.Status {
	case "SAME":
		if prev.SkippedReason != nil {
			_ = m.st.ClearWorkspaceSkip(ctx, chatID)
			m.publishChat(ctx, chatID)
		}
		return nil
	case "SKIP":
		reason := fmt.Sprintf("%s in /workspace, limit %s", formatMB(snap.Size), formatMB(limit))
		if err := m.st.SkipWorkspace(ctx, chatID, reason, snap.Size); err != nil {
			return err
		}
		m.mu.Lock()
		announce := l.wsSkippedFP != snap.Fingerprint
		l.wsSkippedFP = snap.Fingerprint
		m.mu.Unlock()
		slog.Warn("workspace NOT backed up: above the limit", "chat", chatID, "bytes", snap.Size, "files", snap.Files, "limit", limit)
		if announce {
			msg := "Workspace not saved: " + reason + "."
			if prev.SavedAt != nil {
				msg += " Resuming uses the backup from " + prev.SavedAt.Local().Format("2006-01-02 15:04") + "."
			} else {
				msg += " There is no backup yet; if the chat goes idle, the files are lost."
			}
			m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": msg}})
		}
		m.publishChat(ctx, chatID)
		return nil
	}
	if prev.ObjectKey == "" && snap.Files == 0 {
		return nil // never backed up and nothing there
	}
	sum := sha256.Sum256(snap.Archive)
	w := store.Workspace{ChatID: chatID, ObjectKey: workspaceObjectKey(chatID), Fingerprint: snap.Fingerprint,
		WorkspaceInfo: store.WorkspaceInfo{Size: snap.Size, ArchiveSize: int64(len(snap.Archive)), Files: snap.Files, SHA256: hex.EncodeToString(sum[:])}}
	if err := m.blobs.Put(ctx, w.ObjectKey, bytes.NewReader(snap.Archive), w.ArchiveSize, "application/gzip"); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	if err := m.st.PutWorkspace(ctx, w); err != nil {
		return err
	}
	m.mu.Lock()
	l.wsSkippedFP = ""
	m.mu.Unlock()
	slog.Info("workspace backed up", "chat", chatID, "files", snap.Files, "bytes", snap.Size, "archive", w.ArchiveSize)
	m.publishChat(ctx, chatID)
	return nil
}

// restoreWorkspace restores the last backup into a fresh sandbox. If that
// fails, nothing is backed up in this sandbox any more, so that an empty
// workspace does not overwrite the valid backup.
func (m *Manager) restoreWorkspace(ctx context.Context, chatID string, l *live) workspaceRestore {
	if !m.workspaceEnabled() {
		return workspaceRestore{Disabled: true}
	}
	unlock := m.workspaceLock(chatID)
	defer unlock()
	w, err := m.st.GetWorkspace(ctx, chatID)
	if err != nil || w.ObjectKey == "" {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			m.workspaceRestoreFailed(chatID, l, err)
			return workspaceRestore{Err: err}
		}
		return workspaceRestore{}
	}
	res := workspaceRestore{Found: true, Size: w.Size, Files: w.Files}
	failed := func(err error) workspaceRestore {
		m.workspaceRestoreFailed(chatID, l, err)
		res.Err = err
		return res
	}
	ctx, cancel := workspaceContext(ctx)
	defer cancel()
	limit := max(m.workspaceMax(), w.Size)
	rc, _, err := m.blobs.Get(ctx, w.ObjectKey)
	if err != nil {
		return failed(fmt.Errorf("storage: %w", err))
	}
	data, err := io.ReadAll(io.LimitReader(rc, workspaceArchiveCap(limit)+1))
	rc.Close()
	if err != nil {
		return failed(err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != w.SHA256 {
		return failed(errors.New("archive checksum does not match"))
	}
	kept, dropped, err := restoreWorkspaceArchive(ctx, l.slot.Worker, data, 2*limit+16<<20)
	if err != nil {
		return failed(err)
	}
	if dropped > 0 {
		slog.Warn("workspace: entries dropped", "chat", chatID, "dropped", dropped)
	}
	slog.Info("workspace restored", "chat", chatID, "entries", kept, "files", w.Files, "bytes", w.Size)
	return res
}

// workspaceRestore describes the restore on resuming (for the steps in the UI).
type workspaceRestore struct {
	Disabled bool  // AGW_WORKSPACE_MAX_MB=0
	Found    bool  // there was a backup
	Size     int64 // sum of the file sizes according to the backup
	Files    int
	Err      error // restore failed (nothing is backed up in this sandbox)
}

func (m *Manager) workspaceRestoreFailed(chatID string, l *live, err error) {
	m.mu.Lock()
	l.wsNoSave = true
	m.mu.Unlock()
	slog.Error("workspace not restored", "chat", chatID, "err", err)
	m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": "Workspace could not be restored (" + err.Error() +
		"). Nothing is backed up in this sandbox, so that the last backup is kept."}})
}

// workspaceContext: cancelling the request (the user closes the connection)
// does not abort the backup, a deadline (shutdown) does.
func workspaceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(ctx)
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < workspaceTimeout {
		return context.WithDeadline(base, dl)
	}
	return context.WithTimeout(base, workspaceTimeout)
}

// formatMB writes bytes as "1.2 MB" (1 MB = 2^20 bytes like the limit).
func formatMB(n int64) string {
	return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
