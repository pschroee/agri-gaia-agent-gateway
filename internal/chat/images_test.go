package chat

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNormalizeImagePath(t *testing.T) {
	ok := map[string]string{
		"plot.png":                     "/workspace/plot.png",
		"./out/a.png":                  "/workspace/out/a.png",
		"/workspace/plot.png":          "/workspace/plot.png",
		"/workspace/inputs/photo.jpg":  "/workspace/inputs/photo.jpg",
		"/tmp/x.gif":                   "/tmp/x.gif",
		"/home/agent/images/b.webp":    "/home/agent/images/b.webp",
		"file:///workspace/plot.png":   "/workspace/plot.png",
		"/workspace/a/../b.png":        "/workspace/b.png",
		"my plot.png":                  "/workspace/my plot.png",
		"  /workspace/space.png  ":     "/workspace/space.png",
		"/workspace//double//x.png":    "/workspace/double/x.png",
		"sub/folder/./and/../x.png":    "/workspace/sub/folder/x.png",
		"/workspace/umlaut-äöü.png":    "/workspace/umlaut-äöü.png",
		"/workspace/no-extension":      "/workspace/no-extension",
		"/workspace/bracket(1).png":    "/workspace/bracket(1).png",
		"/home/agent/.cache/x/y/z.png": "/home/agent/.cache/x/y/z.png",
	}
	for in, want := range ok {
		got, err := NormalizeImagePath(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "   ", "https://attacker.example/p.png", "http://x/y.png", "//attacker.example/p.png",
		"data:image/png;base64,AAAA", "javascript:alert(1)", "ftp://x/y.png", "file://attacker/x.png",
		"/etc/passwd", "/agent/config/models.json", "/workspace", "/workspace/", "/tmp", "/home/agent",
		"../etc/passwd", "/workspace/../etc/passwd", "../../agent/sessions/s.jsonl", "/proc/1/environ",
		"/workspacex/a.png", "/tmpfoo/a.png", "/home/agentx/a.png", "/opt/agw/x.png",
		"/workspace/a\x00.png", "/workspace/a\n.png", strings.Repeat("a", 1100) + ".png",
	}
	for _, in := range bad {
		if got, err := NormalizeImagePath(in); err == nil {
			t.Errorf("%q was accepted: %q", in, got)
		}
	}
}

func TestDetectImageType(t *testing.T) {
	cases := map[string]string{
		"\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR": "image/png",
		"\xff\xd8\xff\xe0\x00\x10JFIF":        "image/jpeg",
		"GIF89a\x01\x00":                      "image/gif",
		"GIF87a\x01\x00":                      "image/gif",
		"RIFF\x24\x00\x00\x00WEBPVP8 ":        "image/webp",
		"<svg xmlns='http://www.w3.org/2000/svg'><script>alert(1)</script></svg>": "",
		"<html><body>":             "",
		"RIFF\x24\x00\x00\x00WAVE": "",
		"\x89PN":                   "",
		"":                         "",
		"%PDF-1.7":                 "",
	}
	for in, want := range cases {
		if got := DetectImageType([]byte(in)); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestImageRefs(t *testing.T) {
	md := "Here is the chart:\n\n![History](/workspace/plot.png)\n\n" +
		"And relative ![b](out/b.png \"Title\") as well as ![c](<my image.png>).\n" +
		"Foreign: ![x](https://attacker.example/p.png?d=secret) and ![y](data:image/png;base64,AAAA)\n" +
		"Duplicate: ![History again](/workspace/plot.png)\n" +
		"```\n![in code](/workspace/code.png)\n```\n" +
		"Inline `![code too](/workspace/inline.png)` and a link [not an image](/workspace/link.png).\n" +
		"Outside: ![p](/etc/passwd) ![q](../agent/x.png)"
	got := ImageRefs(md)
	want := []string{"/workspace/plot.png", "/workspace/out/b.png", "/workspace/my image.png"}
	if !slices.Equal(got, want) {
		t.Fatalf("references: %q, want %q", got, want)
	}
	if ImageRefs("no images") != nil {
		t.Fatal("empty text returns references")
	}
	var many strings.Builder
	for i := range 50 {
		many.WriteString("![a](/workspace/" + strings.Repeat("x", i+1) + ".png)\n")
	}
	if n := len(ImageRefs(many.String())); n != maxImageRefs {
		t.Fatalf("%d references, want at most %d", n, maxImageRefs)
	}
}

func TestMessageImageKey(t *testing.T) {
	cases := map[string]string{
		`{"role":"assistant","responseId":"chatcmpl-abc_1.2:3","timestamp":1700000000000}`: "chatcmpl-abc_1.2:3",
		`{"role":"assistant","timestamp":1700000000000}`:                                   "ts-1700000000000",
		`{"role":"assistant","responseId":"böse/../id","timestamp":5}`:                     "ts-5",
		`{"role":"assistant"}`: "",
	}
	for in, want := range cases {
		if got := MessageImageKey(json.RawMessage(in)); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
	if !ValidImageMsg("ts-5") || ValidImageMsg("") || ValidImageMsg("a/b") || ValidImageMsg(strings.Repeat("a", 200)) {
		t.Fatal("ValidImageMsg")
	}
}

func TestImageObjectKeyDependsOnMsgAndPath(t *testing.T) {
	a := imageObjectKey("c", "m1", "/workspace/a.png")
	if a == imageObjectKey("c", "m2", "/workspace/a.png") || a == imageObjectKey("c", "m1", "/workspace/b.png") {
		t.Fatal("key does not distinguish answer and path")
	}
	if !strings.HasPrefix(a, "c/images/") || strings.Contains(strings.TrimPrefix(a, "c/images/"), "/") {
		t.Fatalf("key: %q", a)
	}
}

const pngBytes = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-imagedata"

// After an answer with an image reference the orchestrator saves the image; it stays
// retrievable when the chat is idle. Foreign paths, non-images and too large files are not saved.
func TestImagesCapturedAndServedAfterSuspend(t *testing.T) {
	e := setup(t)
	e.m.opt.ImageMaxBytes = 64
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Title: "images"})
	if err != nil {
		t.Fatal(err)
	}
	a := e.agent(0)
	a.mu.Lock()
	a.files["/workspace/plot.png"] = []byte(pngBytes)
	a.files["/workspace/note.png"] = []byte("<svg><script>alert(1)</script></svg>")
	a.files["/workspace/large.png"] = []byte(pngBytes + strings.Repeat("x", 100))
	a.reply = `Done: ![History](plot.png) ![n](/workspace/note.png)`
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "Draw"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	msgs, _ := e.st.Messages(ctx, c.ID)
	msg := MessageImageKey(msgs[len(msgs)-1].Message)
	if msg == "" {
		t.Fatal("no answer ID")
	}
	// Saving runs in the background.
	var saved bool
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !saved; time.Sleep(10 * time.Millisecond) {
		_, err := e.st.GetChatImage(ctx, c.ID, msg, "/workspace/plot.png")
		saved = err == nil
	}
	if !saved {
		t.Fatal("image not saved")
	}
	// The file in the sandbox changes afterwards: the saved version stays authoritative.
	a.mu.Lock()
	a.files["/workspace/plot.png"] = []byte("GIF89a-later")
	a.mu.Unlock()
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	img, rc, err := e.m.OpenImage(ctx, c.ID, msg, "/workspace/plot.png")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if img.ContentType != "image/png" || img.Size != int64(len(pngBytes)) {
		t.Fatalf("image: %+v", img)
	}
	for _, p := range []string{"/workspace/note.png", "/workspace/large.png", "/workspace/missing.png"} {
		if _, _, err := e.m.OpenImage(ctx, c.ID, msg, p); !errors.Is(err, ErrImageUnavailable) {
			t.Errorf("%s: %v, want unavailable", p, err)
		}
	}
	if _, _, err := e.m.OpenImage(ctx, c.ID, msg, "/etc/passwd"); !errors.Is(err, ErrInvalid) {
		t.Errorf("/etc/passwd: %v", err)
	}
	if _, _, err := e.m.OpenImage(ctx, c.ID, "a/b", "/workspace/plot.png"); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid ID: %v", err)
	}
	// No artifacts: display images are a kind of their own.
	if arts, _ := e.st.ListArtifacts(ctx, c.ID); len(arts) != 0 {
		t.Fatalf("artifacts: %+v", arts)
	}
}

// With an active chat, retrieval reads a not yet saved image from the sandbox, with a size
// limit and via realpath (no symlinks out of the permitted locations).
func TestOpenImageReadsFromLiveSandbox(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Title: "live"})
	if err != nil {
		t.Fatal(err)
	}
	a := e.agent(0)
	a.mu.Lock()
	a.files["/tmp/x.png"] = []byte(pngBytes)
	a.mu.Unlock()
	img, rc, err := e.m.OpenImage(ctx, c.ID, "ts-1", "/tmp/x.png")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if img.ContentType != "image/png" {
		t.Fatalf("image: %+v", img)
	}
	a.mu.Lock()
	var script string
	for _, x := range a.execs {
		if strings.Contains(x, "realpath") {
			script = x
		}
	}
	a.mu.Unlock()
	if !strings.Contains(script, "realpath -e") || !strings.Contains(script, "head -c") || !strings.Contains(script, "/workspace/*") {
		t.Fatalf("read script: %q", script)
	}
	if _, _, err := e.m.OpenImage(ctx, "00000000-0000-0000-0000-000000000000", "ts-1", "/tmp/x.png"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign chat: %v", err)
	}
}
