// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Synopsis is the argument part of the command line of the tool, the same text in
// `agw-platform --help` and in the command index of the system note (#198).
func (t Tool) Synopsis() string {
	var b strings.Builder
	afterList := false // after a list everything else only works as an option
	for _, p := range t.Params {
		switch {
		case p.Required && p.Type == "array":
			fmt.Fprintf(&b, " <%s …>", p.Name)
			afterList = true
		case p.Required:
			fmt.Fprintf(&b, " <%s>", p.Name)
		case p.Type == "string" && !afterList:
			fmt.Fprintf(&b, " [%s]", p.Name)
		case p.Name == "query":
			b.WriteString(" [--query k=v …]")
		case p.Type == "array":
			fmt.Fprintf(&b, " [--%s x …]", p.Name)
		default:
			fmt.Fprintf(&b, " [--%s …]", p.Name)
		}
	}
	return b.String()
}

// CLIIndex lists every agw-platform command with its arguments, one line per tool of the
// table, writing ones marked "(approval)". The system note of the CLI binding carries it (#198).
func CLIIndex() string {
	var b strings.Builder
	for _, t := range Tools {
		fmt.Fprintf(&b, "agw-platform %s%s", t.CLI, t.Synopsis())
		if t.Write {
			b.WriteString(" (approval)")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Limits of the REST operation index: the text comes from the platform and goes into the system
// prompt, so it is masked and bounded.
const (
	apiIndexLine     = 240
	apiIndexMax      = 16 << 10
	apiIndexBodyMax  = 2 << 20
	apiIndexRetry    = 60 * time.Second
	apiIndexTimeout  = 10 * time.Second
	apiIndexTruncMsg = "(index truncated)"
)

// APIIndexFrom turns the OpenAPI description of the platform into the index of the REST binding: one
// line per operation as in the digest of api_paths, without the paths blocked for the agent.
func APIIndexFrom(raw []byte) (string, error) {
	digest, err := openAPIDigest(raw, "")
	if err != nil {
		return "", err
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(digest), "\n"), "\n") {
		if strings.HasPrefix(line, "(") { // the closing line is counted anew
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 || PathBlocked(f[1]) {
			continue
		}
		line = VisibleControls(line)
		if r := []rune(line); len(r) > apiIndexLine {
			line = string(r[:apiIndexLine-1]) + "…"
		}
		lines = append(lines, line)
	}
	closing := func(n int) string {
		return fmt.Sprintf("(%d operations; * = required; multipart/form-data bodies do not work via platform_http)", n)
	}
	var b strings.Builder
	kept := 0
	for _, l := range lines {
		if b.Len()+len(l)+1+len(closing(len(lines)))+len(apiIndexTruncMsg)+2 > apiIndexMax {
			b.WriteString(apiIndexTruncMsg + "\n")
			break
		}
		b.WriteString(l + "\n")
		kept++
	}
	b.WriteString(closing(kept) + "\n")
	return b.String(), nil
}

// APIIndex holds the REST operation index of the platform for the system note of the REST binding
// (#198). It is fetched without a token (the OpenAPI description is public) and stays as it is once
// filled, so the system note does not change during plans; an empty result is tried again at most
// once a minute.
type APIIndex struct {
	base string
	hc   *http.Client
	now  func() time.Time

	mu    sync.Mutex
	text  string
	tried time.Time
}

// NewAPIIndex returns the index of the platform at baseURL (AGW_PLATFORM_API_URL); nil without a URL.
func NewAPIIndex(baseURL string, hc *http.Client) *APIIndex {
	if baseURL == "" {
		return nil
	}
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil // the platform is reached directly, as by the platform client
		hc = &http.Client{Transport: tr}
	}
	c := *hc
	c.Timeout = apiIndexTimeout
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &APIIndex{base: strings.TrimRight(baseURL, "/"), hc: &c, now: time.Now}
}

// Get returns the index, or "" while the platform cannot be reached.
func (x *APIIndex) Get(ctx context.Context) string {
	if x == nil {
		return ""
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.text != "" {
		return x.text
	}
	if !x.tried.IsZero() && x.now().Sub(x.tried) < apiIndexRetry {
		return ""
	}
	x.tried = x.now()
	text, err := x.fetch(ctx)
	if err != nil {
		slog.Warn("api index not available", "error", err)
		return ""
	}
	x.text = text
	slog.Info("api index", "bytes", len(text), "sha256", sum(text))
	return text
}

// SHA256 is the checksum of the index text, empty while there is none.
func (x *APIIndex) SHA256() string {
	if x == nil {
		return ""
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return sum(x.text)
}

func sum(s string) string {
	if s == "" {
		return ""
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (x *APIIndex) fetch(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, x.base+"/openapi.json", nil)
	if err != nil {
		return "", err
	}
	resp, err := x.hc.Do(req) // no Authorization header: the description needs no login
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openapi.json: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, apiIndexBodyMax+1))
	if err != nil {
		return "", err
	}
	if len(raw) > apiIndexBodyMax {
		return "", errors.New("openapi.json larger than 2 MiB")
	}
	return APIIndexFrom(raw)
}
