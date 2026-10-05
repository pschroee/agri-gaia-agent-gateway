package chat

// Display images: images the agent shows in an answer via Markdown
// (![description](/workspace/plot.png)). The UI never loads foreign addresses
// (Markdown image exfiltration, Review K1); the orchestrator fetches local paths
// from the sandbox itself, checks them and stores them in S3. This is
// deliberately not an artifact upload: the images only go to the logged-in UI,
// not outside as a result, and therefore need no approval.

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

// ErrImageUnavailable: the image is neither saved nor (still) readable in the sandbox,
// or the file is not a permitted image or is too large.
var ErrImageUnavailable = errors.New("image not available")

// DefaultImageMaxBytes applies when Options.ImageMaxBytes is not set.
const DefaultImageMaxBytes = 10 << 20

// maxImageRefs limits the images that are saved after an answer.
const maxImageRefs = 20

// imageRoots are the locations in the sandbox from which images are read.
var imageRoots = []string{"/workspace", "/tmp", "/home/agent"}

var (
	schemeRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
	imageMsgRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	fenceRe    = regexp.MustCompile("(?s)(```|~~~).*?(```|~~~|$)")
	inlineRe   = regexp.MustCompile("`[^`\n]*`")
	imageRe    = regexp.MustCompile(`!\[[^\]\n]*\]\(\s*(?:<([^>\n]*)>|([^\s)]+))(?:\s+(?:"[^"\n]*"|'[^'\n]*'))?\s*\)`)
)

// NormalizeImagePath checks an image path from an answer and returns it
// absolute and cleaned. Relative paths are taken from /workspace. Only files
// under /workspace, /tmp and /home/agent are allowed; addresses with a scheme
// (http, data, javascript …) and protocol-relative addresses never.
func NormalizeImagePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if rest, ok := strings.CutPrefix(p, "file://"); ok {
		if !strings.HasPrefix(rest, "/") {
			return "", fmt.Errorf("%w: file:// only with an absolute path", ErrInvalid)
		}
		p = rest
	}
	if p == "" || len(p) > 1024 || strings.HasPrefix(p, "//") || schemeRe.MatchString(p) {
		return "", fmt.Errorf("%w: not a local image path", ErrInvalid)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: control character in the path", ErrInvalid)
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
	return "", fmt.Errorf("%w: images only under /workspace, /tmp or /home/agent", ErrInvalid)
}

// DetectImageType recognises PNG, JPEG, GIF and WebP by their magic bytes.
// Anything else (including SVG, which can contain script) returns "".
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

// ImageRefs returns the local image paths from Markdown image references,
// cleaned, without duplicates and without references in code. Foreign addresses
// are dropped.
func ImageRefs(markdown string) []string {
	text := inlineRe.ReplaceAllString(fenceRe.ReplaceAllString(markdown, ""), "")
	var out []string
	seen := map[string]bool{}
	for _, m := range imageRe.FindAllStringSubmatch(text, -1) {
		raw := m[1]
		if raw == "" {
			raw = m[2]
		}
		if strings.Contains(raw, "%") { // like the UI: decode percent-encoding
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

// MessageImageKey is an answer's ID for its images: pi's responseId,
// otherwise ts-<timestamp>. Both are in the message itself and are therefore
// the same live (message_end) and after reloading from Postgres.
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

// ValidImageMsg checks an answer's ID (character set and length).
func ValidImageMsg(s string) bool { return imageMsgRe.MatchString(s) }

func imageObjectKey(chatID, msg, p string) string {
	sum := sha256.Sum256([]byte(msg + "\x00" + p))
	return path.Join(chatID, "images", hex.EncodeToString(sum[:]))
}

// readImageScript reads a file in the sandbox. realpath resolves symlinks; if
// the target lies outside the permitted locations, nothing is read. head -c
// limits the amount (limit + 1, so that "too large" can be detected).
const readImageScript = `p=$(realpath -e -- "$1") || exit 3
case "$p" in /workspace/*|/tmp/*|/home/agent/*) ;; *) echo "path outside the permitted locations" >&2; exit 4 ;; esac
[ -f "$p" ] || exit 5
exec head -c "$2" -- "$p"`

func (m *Manager) imageMax() int64 {
	if m.opt.ImageMaxBytes > 0 {
		return m.opt.ImageMaxBytes
	}
	return DefaultImageMaxBytes
}

// imageLock serialises saving the images per chat. detach waits for it so
// that a save in progress does not vanish with the sandbox.
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

// ensureImage returns the saved image or reads it from the sandbox (a,
// otherwise that of the active chat) and saves it. The first save wins.
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
		return store.ChatImage{}, fmt.Errorf("%w: not saved, and the chat is idle", ErrImageUnavailable)
	}
	max := m.imageMax()
	data, err := execT(a, []string{"sh", "-c", readImageScript, "sh", p, strconv.FormatInt(max+1, 10)}, nil, callTimeout)
	if err != nil {
		return store.ChatImage{}, fmt.Errorf("%w: %s not readable: %v", ErrImageUnavailable, p, err)
	}
	if int64(len(data)) > max {
		return store.ChatImage{}, fmt.Errorf("%w: %s is larger than %d MB", ErrImageUnavailable, p, max>>20)
	}
	ct := DetectImageType(data)
	if ct == "" {
		return store.ChatImage{}, fmt.Errorf("%w: %s is not an image (PNG, JPEG, GIF, WebP)", ErrImageUnavailable, p)
	}
	sum := sha256.Sum256(data)
	im := store.ChatImage{ChatID: chatID, Msg: msg, Path: p, ObjectKey: imageObjectKey(chatID, msg, p),
		ContentType: ct, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	if err := m.blobs.Put(ctx, im.ObjectKey, bytes.NewReader(data), im.Size, ct); err != nil {
		return store.ChatImage{}, fmt.Errorf("storage: %w", err)
	}
	if err := m.st.PutChatImage(ctx, im); err != nil {
		return store.ChatImage{}, err
	}
	slog.Info("display image saved", "chat", chatID, "answer", msg, "path", p, "bytes", im.Size, "type", ct)
	return im, nil
}

// captureImages saves the images a finished answer refers to, in the
// background. Errors are only logged.
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
				slog.Warn("display image not saved", "chat", chatID, "answer", key, "path", p, "err", err)
			}
		}
	}()
}

// OpenImage returns a display image for the UI: from S3, otherwise, if the chat
// is active, from the sandbox (saving it on the way).
func (m *Manager) OpenImage(ctx context.Context, chatID, msg, rawPath string) (store.ChatImage, io.ReadCloser, error) {
	if !ValidImageMsg(msg) {
		return store.ChatImage{}, nil, fmt.Errorf("%w: answer ID", ErrInvalid)
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
		return im, nil, fmt.Errorf("%w: storage: %v", ErrImageUnavailable, err)
	}
	return im, rc, nil
}
