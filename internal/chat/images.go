package chat

// Anzeige-Bilder: Bilder, die der Agent in einer Antwort per Markdown zeigt
// (![Beschreibung](/workspace/plot.png)). Die UI lädt nie fremde Adressen
// (Markdown-Image-Exfiltration, Review K1); lokale Pfade aus der Sandbox holt
// der Orchestrator selbst, prüft sie und legt sie in S3 ab. Das ist bewusst kein
// Artefakt-Upload: Die Bilder gehen nur an die angemeldete UI, nicht als
// Ergebnis nach draußen, und brauchen deshalb keine Bestätigung.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"agw/internal/store"
)

// ErrImageUnavailable: Das Bild ist weder gesichert noch (noch) in der Sandbox lesbar,
// oder die Datei ist kein erlaubtes Bild bzw. zu groß.
var ErrImageUnavailable = errors.New("Bild nicht verfügbar")

// DefaultImageMaxBytes gilt, wenn Options.ImageMaxBytes nicht gesetzt ist.
const DefaultImageMaxBytes = 10 << 20

// maxImageRefs begrenzt die Bilder, die nach einer Antwort gesichert werden.
const maxImageRefs = 20

// imageRoots sind die Orte in der Sandbox, aus denen Bilder gelesen werden.
var imageRoots = []string{"/workspace", "/tmp", "/home/agent"}

var (
	schemeRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
	imageMsgRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	fenceRe    = regexp.MustCompile("(?s)(```|~~~).*?(```|~~~|$)")
	inlineRe   = regexp.MustCompile("`[^`\n]*`")
	imageRe    = regexp.MustCompile(`!\[[^\]\n]*\]\(\s*(?:<([^>\n]*)>|([^\s)]+))(?:\s+(?:"[^"\n]*"|'[^'\n]*'))?\s*\)`)
)

// NormalizeImagePath prüft einen Bildpfad aus einer Antwort und liefert ihn
// absolut und bereinigt. Relative Pfade gelten ab /workspace. Erlaubt sind nur
// Dateien unter /workspace, /tmp und /home/agent; Adressen mit Schema (http,
// data, javascript …) und protokollrelative Adressen nie.
func NormalizeImagePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if rest, ok := strings.CutPrefix(p, "file://"); ok {
		if !strings.HasPrefix(rest, "/") {
			return "", fmt.Errorf("%w: file:// nur mit absolutem Pfad", ErrInvalid)
		}
		p = rest
	}
	if p == "" || len(p) > 1024 || strings.HasPrefix(p, "//") || schemeRe.MatchString(p) {
		return "", fmt.Errorf("%w: kein lokaler Bildpfad", ErrInvalid)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: Steuerzeichen im Pfad", ErrInvalid)
		}
	}
	if !strings.HasPrefix(p, "/") {
		p = "/workspace/" + p
	}
	p = path.Clean(p)
	for _, root := range imageRoots {
		if strings.HasPrefix(p, root+"/") {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: Bilder nur unter /workspace, /tmp oder /home/agent", ErrInvalid)
}

// DetectImageType erkennt PNG, JPEG, GIF und WebP an den Magic Bytes. Alles
// andere (auch SVG, das Skript enthalten kann) liefert "".
func DetectImageType(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "image/gif"
	case len(b) >= 12 && bytes.HasPrefix(b, []byte("RIFF")) && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// ImageRefs liefert die lokalen Bildpfade aus Markdown-Bildverweisen, bereinigt,
// ohne Doppelte und ohne Verweise in Code. Fremde Adressen fallen heraus.
func ImageRefs(markdown string) []string {
	text := inlineRe.ReplaceAllString(fenceRe.ReplaceAllString(markdown, ""), "")
	var out []string
	seen := map[string]bool{}
	for _, m := range imageRe.FindAllStringSubmatch(text, -1) {
		raw := m[1]
		if raw == "" {
			raw = m[2]
		}
		if strings.Contains(raw, "%") { // wie die UI: Prozentkodierung auflösen
			if d, err := url.PathUnescape(raw); err == nil {
				raw = d
			}
		}
		p, err := NormalizeImagePath(raw)
		if err != nil || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) == maxImageRefs {
			break
		}
	}
	return out
}

// MessageImageKey ist die Kennung einer Antwort für ihre Bilder: pis
// responseId, sonst ts-<timestamp>. Beide stehen in der Nachricht selbst und
// sind damit live (message_end) und nach dem Neuladen aus Postgres gleich.
func MessageImageKey(msg json.RawMessage) string {
	var m struct {
		ResponseID string `json:"responseId"`
		Timestamp  int64  `json:"timestamp"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return ""
	}
	if ValidImageMsg(m.ResponseID) {
		return m.ResponseID
	}
	if m.Timestamp > 0 {
		return "ts-" + strconv.FormatInt(m.Timestamp, 10)
	}
	return ""
}

// ValidImageMsg prüft die Kennung einer Antwort (Zeichenvorrat und Länge).
func ValidImageMsg(s string) bool { return imageMsgRe.MatchString(s) }

func imageObjectKey(chatID, msg, p string) string {
	sum := sha256.Sum256([]byte(msg + "\x00" + p))
	return path.Join(chatID, "images", hex.EncodeToString(sum[:]))
}

// readImageScript liest eine Datei in der Sandbox. realpath löst Symlinks auf;
// liegt das Ziel außerhalb der erlaubten Orte, wird nichts gelesen. head -c
// begrenzt die Menge (Grenze + 1, damit „zu groß“ erkennbar ist).
const readImageScript = `p=$(realpath -e -- "$1") || exit 3
case "$p" in /workspace/*|/tmp/*|/home/agent/*) ;; *) echo "Pfad außerhalb der erlaubten Orte" >&2; exit 4 ;; esac
[ -f "$p" ] || exit 5
exec head -c "$2" -- "$p"`

func (m *Manager) imageMax() int64 {
	if m.opt.ImageMaxBytes > 0 {
		return m.opt.ImageMaxBytes
	}
	return DefaultImageMaxBytes
}

// imageLock serialisiert das Sichern der Bilder je Chat. detach wartet darauf,
// damit eine laufende Sicherung nicht mit der Sandbox verschwindet.
func (m *Manager) imageLock(chatID string) func() {
	m.mu.Lock()
	l, ok := m.imgMu[chatID]
	if !ok {
		l = &sync.Mutex{}
		m.imgMu[chatID] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// ensureImage liefert das gesicherte Bild oder liest es aus der Sandbox (a,
// sonst die des aktiven Chats) und sichert es. Die erste Sicherung gilt.
func (m *Manager) ensureImage(ctx context.Context, chatID, msg, p string, a Agent) (store.ChatImage, error) {
	unlock := m.imageLock(chatID)
	defer unlock()
	if im, err := m.st.GetChatImage(ctx, chatID, msg, p); err == nil {
		return im, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return im, err
	}
	if a == nil {
		m.mu.Lock()
		if l := m.live[chatID]; l != nil {
			a = l.slot.Worker
		}
		m.mu.Unlock()
	}
	if a == nil {
		return store.ChatImage{}, fmt.Errorf("%w: nicht gesichert, und der Chat ruht", ErrImageUnavailable)
	}
	max := m.imageMax()
	data, err := execT(a, []string{"sh", "-c", readImageScript, "sh", p, strconv.FormatInt(max+1, 10)}, nil, callTimeout)
	if err != nil {
		return store.ChatImage{}, fmt.Errorf("%w: %s nicht lesbar: %v", ErrImageUnavailable, p, err)
	}
	if int64(len(data)) > max {
		return store.ChatImage{}, fmt.Errorf("%w: %s ist größer als %d MB", ErrImageUnavailable, p, max>>20)
	}
	ct := DetectImageType(data)
	if ct == "" {
		return store.ChatImage{}, fmt.Errorf("%w: %s ist kein Bild (PNG, JPEG, GIF, WebP)", ErrImageUnavailable, p)
	}
	sum := sha256.Sum256(data)
	im := store.ChatImage{ChatID: chatID, Msg: msg, Path: p, ObjectKey: imageObjectKey(chatID, msg, p),
		ContentType: ct, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	if err := m.blobs.Put(ctx, im.ObjectKey, bytes.NewReader(data), im.Size, ct); err != nil {
		return store.ChatImage{}, fmt.Errorf("Ablage: %w", err)
	}
	if err := m.st.PutChatImage(ctx, im); err != nil {
		return store.ChatImage{}, err
	}
	slog.Info("Anzeige-Bild gesichert", "chat", chatID, "antwort", msg, "pfad", p, "bytes", im.Size, "typ", ct)
	return im, nil
}

// captureImages sichert nach einer fertigen Antwort die Bilder, auf die sie
// verweist, im Hintergrund. Fehler werden nur protokolliert.
func (m *Manager) captureImages(chatID string, a Agent, msg json.RawMessage) {
	var body struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(msg, &body) != nil {
		return
	}
	var text strings.Builder
	for _, b := range body.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
			text.WriteString("\n")
		}
	}
	refs := ImageRefs(text.String())
	key := MessageImageKey(msg)
	if len(refs) == 0 || key == "" {
		return
	}
	go func() {
		ctx := context.Background()
		for _, p := range refs {
			if _, err := m.ensureImage(ctx, chatID, key, p, a); err != nil {
				slog.Warn("Anzeige-Bild nicht gesichert", "chat", chatID, "antwort", key, "pfad", p, "fehler", err)
			}
		}
	}()
}

// OpenImage liefert ein Anzeige-Bild für die UI: aus S3, sonst bei aktivem Chat
// aus der Sandbox (und sichert es dabei).
func (m *Manager) OpenImage(ctx context.Context, chatID, msg, rawPath string) (store.ChatImage, io.ReadCloser, error) {
	if !ValidImageMsg(msg) {
		return store.ChatImage{}, nil, fmt.Errorf("%w: Kennung der Antwort", ErrInvalid)
	}
	p, err := NormalizeImagePath(rawPath)
	if err != nil {
		return store.ChatImage{}, nil, err
	}
	if _, err := m.st.GetChat(ctx, chatID); err != nil {
		return store.ChatImage{}, nil, err
	}
	im, err := m.ensureImage(ctx, chatID, msg, p, nil)
	if err != nil {
		return im, nil, err
	}
	rc, _, err := m.blobs.Get(ctx, im.ObjectKey)
	if err != nil {
		return im, nil, fmt.Errorf("%w: Ablage: %v", ErrImageUnavailable, err)
	}
	return im, rc, nil
}
