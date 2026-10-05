package chat

// Arbeitsbereich je Chat: /workspace überlebt das Ruhen. Nach jedem
// abgeschlossenen Lauf und beim Ruhen packt der Orchestrator /workspace in der
// Sandbox (ohne inputs/ und ohne Paket- und Cache-Ordner) als tar.gz und legt
// es in S3 unter <chat>/workspace.tar.gz ab; beim Fortsetzen spielt er es in
// die frische Sandbox ein, bevor der erste Auftrag kommt.
//
// Das Archiv stammt aus der Sandbox desselben Chats und ist damit vom Agenten
// gestaltbar. Vor dem Einspielen liest der Orchestrator es deshalb selbst und
// reicht nur unbedenkliche Einträge weiter (normale Dateien, Ordner, Symlinks,
// harte Links innerhalb des Archivs; keine absoluten Pfade, kein "..", nichts
// unter einem Symlink, nichts unter inputs/). Ausgepackt wird als Agent-Nutzer
// ohne Besitzer aus dem Archiv.

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

// DefaultWorkspaceMaxBytes gilt, wenn Options.WorkspaceMaxBytes 0 ist.
const DefaultWorkspaceMaxBytes = 200 << 20

// workspaceTimeout begrenzt Packen und Auspacken.
const workspaceTimeout = 2 * time.Minute

// WorkspaceExcludes sind Ordnernamen, die nie gesichert werden, gleich in
// welcher Tiefe: nachinstallierte Pakete und Caches (Entscheidung des Nutzers:
// Pakete bleiben nicht erhalten). inputs/ auf oberster Ebene kommt ohnehin aus
// den Eingaben des Chats.
var WorkspaceExcludes = []string{"node_modules", ".venv", "__pycache__", ".cache"}

func workspaceObjectKey(chatID string) string { return path.Join(chatID, "workspace.tar.gz") }

// workspaceSaveScript (bash, als Agent-Nutzer) ermittelt Fingerabdruck,
// Dateizahl und Größe von /workspace und packt es, wenn es sich seit der letzten
// Sicherung geändert hat und unter der Grenze liegt. Erste Zeile der Ausgabe:
//
//	SAME <fp> <dateien> <bytes>   unverändert
//	SKIP <fp> <dateien> <bytes>   über der Grenze, nichts gepackt
//	DATA <fp> <dateien> <bytes>   danach folgt das tar.gz
//
// Der Fingerabdruck zählt nur Dateien, Ordner und Symlinks (anderes wird beim
// Einspielen verworfen), bei Ordnern ohne Größe und Zeit (die ändern sich auch
// durch ausgeschlossene Unterordner wie __pycache__); --format=posix erhält die Zeiten auf die Nanosekunde,
// damit der Stand nach dem Einspielen wieder denselben Fingerabdruck hat.
// head -c begrenzt die Ausgabe hart (wächst eine Datei während des Packens).
// tar-Status 1 (Datei während des Lesens geändert) gilt als Erfolg.
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

// workspaceRestoreScript packt einen (vom Orchestrator gefilterten) tar-Strom
// nach /workspace aus, ohne Besitzer aus dem Archiv.
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

// wsSnapshot ist das Ergebnis des Sicherungsskripts.
type wsSnapshot struct {
	Status      string // SAME, SKIP, DATA
	Fingerprint string
	Files       int
	Size        int64
	Archive     []byte // nur bei DATA
}

// workspaceArchiveCap ist die harte Obergrenze für das Archiv (tar-Köpfe,
// Auffüllung auf 512 Bytes je Datei, Wachstum während des Packens).
func workspaceArchiveCap(limit int64) int64 { return 2*limit + 16<<20 }

// snapshotWorkspace führt das Sicherungsskript in der Sandbox aus.
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
		return s, fmt.Errorf("unerwartete Ausgabe des Sicherungsskripts: %q", truncate(string(head), 200))
	}
	s.Status, s.Fingerprint = f[0], f[1]
	s.Files, _ = strconv.Atoi(f[2])
	s.Size, _ = strconv.ParseInt(f[3], 10, 64)
	switch s.Status {
	case "SAME", "SKIP":
		return s, err
	case "DATA":
	default:
		return s, fmt.Errorf("unerwarteter Status %q", s.Status)
	}
	if int64(len(rest)) > capBytes {
		// Während des Packens gewachsen: wie über der Grenze behandeln.
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

// filterWorkspaceArchive liest ein tar.gz und schreibt die unbedenklichen
// Einträge als unkomprimiertes tar nach w. Liefert die Zahl der übernommenen
// und der verworfenen Einträge. maxContent begrenzt die Summe der Dateigrößen
// (Schutz gegen Archive, die beim Entpacken übergroß werden).
func filterWorkspaceArchive(r io.Reader, w io.Writer, maxContent int64) (kept, dropped int, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, 0, fmt.Errorf("kein gzip: %w", err)
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
			return kept, dropped, fmt.Errorf("Archiv beschädigt: %w", err)
		}
		name, ok := safeArchivePath(h.Name)
		if !ok || underSymlink(name, symlinks) {
			dropped++
			continue
		}
		if name == "." {
			continue // Wurzel: /workspace gibt es schon
		}
		out := &tar.Header{Name: name, Mode: h.Mode & 0o7777 &^ 0o6000, ModTime: h.ModTime, Format: tar.FormatPAX}
		switch h.Typeflag {
		case tar.TypeDir:
			out.Typeflag = tar.TypeDir
			out.Name += "/"
		case tar.TypeReg, tar.TypeRegA:
			total += h.Size
			if total > maxContent {
				return kept, dropped, fmt.Errorf("Inhalt größer als %d Bytes", maxContent)
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
		default: // FIFOs, Geräte und alles andere
			dropped++
			continue
		}
		// Ein Name, der schon als anderer Typ vorkam, wird nicht ersetzt.
		if symlinks[name] || (regular[name] && out.Typeflag != tar.TypeReg) {
			dropped++
			continue
		}
		if err := tw.WriteHeader(out); err != nil {
			return kept, dropped, err
		}
		if out.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, tr, h.Size); err != nil {
				return kept, dropped, fmt.Errorf("Archiv beschädigt: %w", err)
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

// safeArchivePath bereinigt einen Namen aus dem Archiv. Abgelehnt: absolute
// Pfade, "..", Steuerzeichen und alles unter inputs/ (kommt aus den Eingaben).
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

// underSymlink: liegt der Pfad unter einem Symlink aus demselben Archiv? Dann
// würde tar beim Auspacken dem Symlink folgen.
func underSymlink(p string, symlinks map[string]bool) bool {
	for i := 0; i < len(p); i++ {
		if p[i] == '/' && symlinks[p[:i]] {
			return true
		}
	}
	return false
}

// restoreWorkspaceArchive filtert das Archiv und packt es in der Sandbox aus.
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
	pr.CloseWithError(errors.New("abgebrochen"))
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

// workspaceLock serialisiert Sichern und Einspielen je Chat.
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

// saveWorkspace sichert /workspace der Sandbox des Chats, falls es sich
// geändert hat. Über der Grenze wird nichts gesichert; die letzte gültige
// Sicherung bleibt, und der Chat bekommt einen Hinweis.
func (m *Manager) saveWorkspace(ctx context.Context, chatID string, l *live) error {
	if !m.workspaceEnabled() {
		return nil
	}
	unlock := m.workspaceLock(chatID)
	defer unlock()
	select {
	case <-l.stop:
		return nil // Sandbox schon abgebaut
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
		reason := fmt.Sprintf("%s in /workspace, Grenze %s", formatMB(snap.Size), formatMB(limit))
		if err := m.st.SkipWorkspace(ctx, chatID, reason, snap.Size); err != nil {
			return err
		}
		m.mu.Lock()
		announce := l.wsSkippedFP != snap.Fingerprint
		l.wsSkippedFP = snap.Fingerprint
		m.mu.Unlock()
		slog.Warn("Arbeitsbereich NICHT gesichert: über der Grenze", "chat", chatID, "bytes", snap.Size, "dateien", snap.Files, "grenze", limit)
		if announce {
			msg := "Arbeitsbereich nicht gesichert: " + reason + "."
			if prev.SavedAt != nil {
				msg += " Beim Fortsetzen gilt die Sicherung von " + prev.SavedAt.Local().Format("02.01. 15:04") + "."
			} else {
				msg += " Es gibt noch keine Sicherung; ruht der Chat, gehen die Dateien verloren."
			}
			m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": msg}})
		}
		m.publishChat(ctx, chatID)
		return nil
	}
	if prev.ObjectKey == "" && snap.Files == 0 {
		return nil // nie gesichert und nichts da
	}
	sum := sha256.Sum256(snap.Archive)
	w := store.Workspace{ChatID: chatID, ObjectKey: workspaceObjectKey(chatID), Fingerprint: snap.Fingerprint,
		WorkspaceInfo: store.WorkspaceInfo{Size: snap.Size, ArchiveSize: int64(len(snap.Archive)), Files: snap.Files, SHA256: hex.EncodeToString(sum[:])}}
	if err := m.blobs.Put(ctx, w.ObjectKey, bytes.NewReader(snap.Archive), w.ArchiveSize, "application/gzip"); err != nil {
		return fmt.Errorf("Ablage: %w", err)
	}
	if err := m.st.PutWorkspace(ctx, w); err != nil {
		return err
	}
	m.mu.Lock()
	l.wsSkippedFP = ""
	m.mu.Unlock()
	slog.Info("Arbeitsbereich gesichert", "chat", chatID, "dateien", snap.Files, "bytes", snap.Size, "archiv", w.ArchiveSize)
	m.publishChat(ctx, chatID)
	return nil
}

// restoreWorkspace spielt die letzte Sicherung in eine frische Sandbox ein.
// Scheitert das, wird in dieser Sandbox nicht mehr gesichert, damit ein leerer
// Arbeitsbereich die gültige Sicherung nicht überschreibt.
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
		return failed(fmt.Errorf("Ablage: %w", err))
	}
	data, err := io.ReadAll(io.LimitReader(rc, workspaceArchiveCap(limit)+1))
	rc.Close()
	if err != nil {
		return failed(err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != w.SHA256 {
		return failed(errors.New("Prüfsumme des Archivs stimmt nicht"))
	}
	kept, dropped, err := restoreWorkspaceArchive(ctx, l.slot.Worker, data, 2*limit+16<<20)
	if err != nil {
		return failed(err)
	}
	if dropped > 0 {
		slog.Warn("Arbeitsbereich: Einträge verworfen", "chat", chatID, "verworfen", dropped)
	}
	slog.Info("Arbeitsbereich eingespielt", "chat", chatID, "einträge", kept, "dateien", w.Files, "bytes", w.Size)
	return res
}

// workspaceRestore beschreibt das Einspielen beim Fortsetzen (für die Schritte in der UI).
type workspaceRestore struct {
	Disabled bool  // AGW_WORKSPACE_MAX_MB=0
	Found    bool  // es gab eine Sicherung
	Size     int64 // Summe der Dateigrößen laut Sicherung
	Files    int
	Err      error // Einspielen gescheitert (in dieser Sandbox wird nicht gesichert)
}

func (m *Manager) workspaceRestoreFailed(chatID string, l *live, err error) {
	m.mu.Lock()
	l.wsNoSave = true
	m.mu.Unlock()
	slog.Error("Arbeitsbereich nicht eingespielt", "chat", chatID, "fehler", err)
	m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": "Arbeitsbereich konnte nicht wiederhergestellt werden (" + err.Error() +
		"). In dieser Sandbox wird nicht gesichert, damit die letzte Sicherung erhalten bleibt."}})
}

// workspaceContext: Ein Abbruch der Anfrage (Nutzer schließt die Verbindung)
// bricht die Sicherung nicht ab, eine Frist (Herunterfahren) schon.
func workspaceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(ctx)
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < workspaceTimeout {
		return context.WithDeadline(base, dl)
	}
	return context.WithTimeout(base, workspaceTimeout)
}

// formatMB schreibt Bytes als „1,2 MB“ (Dezimalkomma, 1 MB = 2^20 Bytes wie die Grenze).
func formatMB(n int64) string {
	return strings.Replace(strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64), ".", ",", 1) + " MB"
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
