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
		"/workspace/inputs/foto.jpg":   "/workspace/inputs/foto.jpg",
		"/tmp/x.gif":                   "/tmp/x.gif",
		"/home/agent/bilder/b.webp":    "/home/agent/bilder/b.webp",
		"file:///workspace/plot.png":   "/workspace/plot.png",
		"/workspace/a/../b.png":        "/workspace/b.png",
		"my plot.png":                  "/workspace/my plot.png",
		"  /workspace/leer.png  ":      "/workspace/leer.png",
		"/workspace//doppelt//x.png":   "/workspace/doppelt/x.png",
		"unter/ordner/./und/../x.png":  "/workspace/unter/ordner/x.png",
		"/workspace/umlaut-äöü.png":    "/workspace/umlaut-äöü.png",
		"/workspace/ohne-endung":       "/workspace/ohne-endung",
		"/workspace/klammer(1).png":    "/workspace/klammer(1).png",
		"/home/agent/.cache/x/y/z.png": "/home/agent/.cache/x/y/z.png",
	}
	for in, want := range ok {
		got, err := NormalizeImagePath(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, erwartet %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "   ", "https://angreifer.example/p.png", "http://x/y.png", "//angreifer.example/p.png",
		"data:image/png;base64,AAAA", "javascript:alert(1)", "ftp://x/y.png", "file://angreifer/x.png",
		"/etc/passwd", "/agent/config/models.json", "/workspace", "/workspace/", "/tmp", "/home/agent",
		"../etc/passwd", "/workspace/../etc/passwd", "../../agent/sessions/s.jsonl", "/proc/1/environ",
		"/workspacex/a.png", "/tmpfoo/a.png", "/home/agentx/a.png", "/opt/agw/x.png",
		"/workspace/a\x00.png", "/workspace/a\n.png", strings.Repeat("a", 1100) + ".png",
	}
	for _, in := range bad {
		if got, err := NormalizeImagePath(in); err == nil {
			t.Errorf("%q wurde angenommen: %q", in, got)
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
			t.Errorf("%q: %q, erwartet %q", in, got, want)
		}
	}
}

func TestImageRefs(t *testing.T) {
	md := "Hier die Grafik:\n\n![Verlauf](/workspace/plot.png)\n\n" +
		"Und relativ ![b](out/b.png \"Titel\") sowie ![c](<mein bild.png>).\n" +
		"Fremd: ![x](https://angreifer.example/p.png?d=geheim) und ![y](data:image/png;base64,AAAA)\n" +
		"Doppelt: ![Verlauf nochmal](/workspace/plot.png)\n" +
		"```\n![im Code](/workspace/code.png)\n```\n" +
		"Inline `![auch Code](/workspace/inline.png)` und ein Link [kein Bild](/workspace/link.png).\n" +
		"Außerhalb: ![p](/etc/passwd) ![q](../agent/x.png)"
	got := ImageRefs(md)
	want := []string{"/workspace/plot.png", "/workspace/out/b.png", "/workspace/mein bild.png"}
	if !slices.Equal(got, want) {
		t.Fatalf("Verweise: %q, erwartet %q", got, want)
	}
	if ImageRefs("ohne Bilder") != nil {
		t.Fatal("leerer Text liefert Verweise")
	}
	var many strings.Builder
	for i := range 50 {
		many.WriteString("![a](/workspace/" + strings.Repeat("x", i+1) + ".png)\n")
	}
	if n := len(ImageRefs(many.String())); n != maxImageRefs {
		t.Fatalf("%d Verweise, erwartet höchstens %d", n, maxImageRefs)
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
			t.Errorf("%s: %q, erwartet %q", in, got, want)
		}
	}
	if !ValidImageMsg("ts-5") || ValidImageMsg("") || ValidImageMsg("a/b") || ValidImageMsg(strings.Repeat("a", 200)) {
		t.Fatal("ValidImageMsg")
	}
}

func TestImageObjectKeyDependsOnMsgAndPath(t *testing.T) {
	a := imageObjectKey("c", "m1", "/workspace/a.png")
	if a == imageObjectKey("c", "m2", "/workspace/a.png") || a == imageObjectKey("c", "m1", "/workspace/b.png") {
		t.Fatal("Schlüssel unterscheidet Antwort und Pfad nicht")
	}
	if !strings.HasPrefix(a, "c/images/") || strings.Contains(strings.TrimPrefix(a, "c/images/"), "/") {
		t.Fatalf("Schlüssel: %q", a)
	}
}

const pngBytes = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-Bilddaten"

// Nach einer Antwort mit Bildverweis sichert der Orchestrator das Bild; es bleibt
// abrufbar, wenn der Chat ruht. Fremde Pfade, Nicht-Bilder und zu große Dateien nicht.
func TestImagesCapturedAndServedAfterSuspend(t *testing.T) {
	e := setup(t)
	e.m.opt.ImageMaxBytes = 64
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Title: "bilder"})
	if err != nil {
		t.Fatal(err)
	}
	a := e.agent(0)
	a.mu.Lock()
	a.files["/workspace/plot.png"] = []byte(pngBytes)
	a.files["/workspace/notiz.png"] = []byte("<svg><script>alert(1)</script></svg>")
	a.files["/workspace/gross.png"] = []byte(pngBytes + strings.Repeat("x", 100))
	a.reply = `Fertig: ![Verlauf](plot.png) ![n](/workspace/notiz.png)`
	a.mu.Unlock()
	if _, err := e.m.Send(ctx, c.ID, "Zeichne"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	msgs, _ := e.st.Messages(ctx, c.ID)
	msg := MessageImageKey(msgs[len(msgs)-1].Message)
	if msg == "" {
		t.Fatal("keine Kennung der Antwort")
	}
	// Die Sicherung läuft im Hintergrund.
	var saved bool
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !saved; time.Sleep(10 * time.Millisecond) {
		_, err := e.st.GetChatImage(ctx, c.ID, msg, "/workspace/plot.png")
		saved = err == nil
	}
	if !saved {
		t.Fatal("Bild nicht gesichert")
	}
	// Die Datei in der Sandbox ändert sich danach: maßgeblich bleibt die gesicherte Fassung.
	a.mu.Lock()
	a.files["/workspace/plot.png"] = []byte("GIF89a-später")
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
		t.Fatalf("Bild: %+v", img)
	}
	for _, p := range []string{"/workspace/notiz.png", "/workspace/gross.png", "/workspace/fehlt.png"} {
		if _, _, err := e.m.OpenImage(ctx, c.ID, msg, p); !errors.Is(err, ErrImageUnavailable) {
			t.Errorf("%s: %v, erwartet nicht verfügbar", p, err)
		}
	}
	if _, _, err := e.m.OpenImage(ctx, c.ID, msg, "/etc/passwd"); !errors.Is(err, ErrInvalid) {
		t.Errorf("/etc/passwd: %v", err)
	}
	if _, _, err := e.m.OpenImage(ctx, c.ID, "a/b", "/workspace/plot.png"); !errors.Is(err, ErrInvalid) {
		t.Errorf("ungültige Kennung: %v", err)
	}
	// Keine Artefakte: Anzeige-Bilder sind eine eigene Art.
	if arts, _ := e.st.ListArtifacts(ctx, c.ID); len(arts) != 0 {
		t.Fatalf("Artefakte: %+v", arts)
	}
}

// Bei aktivem Chat liest der Abruf ein noch nicht gesichertes Bild aus der Sandbox,
// mit Größengrenze und über realpath (keine Symlinks aus den erlaubten Orten hinaus).
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
		t.Fatalf("Bild: %+v", img)
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
		t.Fatalf("Leseskript: %q", script)
	}
	if _, _, err := e.m.OpenImage(ctx, "00000000-0000-0000-0000-000000000000", "ts-1", "/tmp/x.png"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fremder Chat: %v", err)
	}
}
