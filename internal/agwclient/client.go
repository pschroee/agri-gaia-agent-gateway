package agwclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DefaultURL is the orchestrator's address when nothing else is given.
const DefaultURL = "http://127.0.0.1:18480"

// ErrNoSlot: POST /api/chats with 503 – the pool has no free slot.
var ErrNoSlot = errors.New("No free slot in the pool")

// APIError is an error response from the server ({"error": …}).
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("server responded with %d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("server responded with %d: %s", e.Status, e.Message)
}

type Client struct {
	BaseURL string
	HTTP    *http.Client
	Token   string // API token (AGW_API_TOKEN), sent as bearer
}

func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultURL
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{}, Token: os.Getenv("AGW_API_TOKEN")}
}

func esc(s string) string { return url.PathEscape(s) }

func (c *Client) request(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err == nil && c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("orchestrator at %s not reachable: %w", c.BaseURL, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		ae := &APIError{Status: resp.StatusCode}
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			ae.Message = e.Error
		} else {
			ae.Message = strings.TrimSpace(string(b))
		}
		return nil, ae
	}
	return resp, nil
}

// Do performs a JSON call. in (if not nil) is sent as JSON, the response is decoded
// into out (if not nil).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.request(ctx, method, path, body, ct)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("response from %s %s not readable: %w", method, path, err)
	}
	return nil
}

func (c *Client) Models(ctx context.Context) (out []Model, err error) {
	return out, c.Do(ctx, "GET", "/api/models", nil, &out)
}

func (c *Client) Variants(ctx context.Context) (out []Variant, err error) {
	return out, c.Do(ctx, "GET", "/api/variants", nil, &out)
}

func (c *Client) Config(ctx context.Context) (out Config, err error) {
	return out, c.Do(ctx, "GET", "/api/config", nil, &out)
}

func (c *Client) Pool(ctx context.Context) (out Pool, err error) {
	return out, c.Do(ctx, "GET", "/api/pool", nil, &out)
}

func (c *Client) Chats(ctx context.Context) (out []Chat, err error) {
	return out, c.Do(ctx, "GET", "/api/chats", nil, &out)
}

// CreateChat takes a slot from the pool. On 503 the error is ErrNoSlot (errors.Is).
func (c *Client) CreateChat(ctx context.Context, req CreateChatRequest) (out Chat, err error) {
	err = c.Do(ctx, "POST", "/api/chats", req, &out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusServiceUnavailable {
		if ae.Message != "" {
			return out, fmt.Errorf("%w (%s)", ErrNoSlot, ae.Message)
		}
		return out, ErrNoSlot
	}
	return out, err
}

func (c *Client) Chat(ctx context.Context, id string) (out ChatDetail, err error) {
	return out, c.Do(ctx, "GET", "/api/chats/"+esc(id), nil, &out)
}

func (c *Client) Send(ctx context.Context, id, text string) (out SendResult, err error) {
	return out, c.Do(ctx, "POST", "/api/chats/"+esc(id)+"/messages", map[string]string{"text": text}, &out)
}

// Queue returns the queued messages.
func (c *Client) Queue(ctx context.Context, id string) (out []QueueEntry, err error) {
	return out, c.Do(ctx, "GET", "/api/chats/"+esc(id)+"/queue", nil, &out)
}

// FlushQueue hands over held-back messages now (409 if the agent is working).
func (c *Client) FlushQueue(ctx context.Context, id string) (out SendResult, err error) {
	return out, c.Do(ctx, "POST", "/api/chats/"+esc(id)+"/queue/send", nil, &out)
}

// Unqueue removes a queued message (409 if already handed over).
func (c *Client) Unqueue(ctx context.Context, id, entry string) error {
	return c.Do(ctx, "DELETE", "/api/chats/"+esc(id)+"/queue/"+esc(entry), nil, nil)
}

// Background returns the background tasks of a chat.
func (c *Client) Background(ctx context.Context, id string) (out []BackgroundTask, err error) {
	return out, c.Do(ctx, "GET", "/api/chats/"+esc(id)+"/background", nil, &out)
}

// StopBackground stops a background task (409 if it is not running).
func (c *Client) StopBackground(ctx context.Context, id, bg string) (out BackgroundTask, err error) {
	return out, c.Do(ctx, "POST", "/api/chats/"+esc(id)+"/background/"+esc(bg)+"/stop", nil, &out)
}

func (c *Client) chatAction(ctx context.Context, id, action string, in any) (out Chat, err error) {
	return out, c.Do(ctx, "POST", "/api/chats/"+esc(id)+"/"+action, in, &out)
}

func (c *Client) Abort(ctx context.Context, id string) (Chat, error) {
	return c.chatAction(ctx, id, "abort", nil)
}

func (c *Client) Suspend(ctx context.Context, id string) (Chat, error) {
	return c.chatAction(ctx, id, "suspend", nil)
}

func (c *Client) SetInternet(ctx context.Context, id string, enabled bool) (Chat, error) {
	return c.chatAction(ctx, id, "internet", map[string]bool{"enabled": enabled})
}

func (c *Client) SetAutoCompact(ctx context.Context, id string, enabled bool) (Chat, error) {
	return c.chatAction(ctx, id, "autocompact", map[string]bool{"enabled": enabled})
}

// LLMCalls returns all model calls of the chat recorded at the LLM proxy.
func (c *Client) LLMCalls(ctx context.Context, id string) (out []LLMCall, err error) {
	return out, c.Do(ctx, "GET", "/api/chats/"+esc(id)+"/llm_calls", nil, &out)
}

// ToolExecutions returns the reconciliation requested ↔ executed per toolCallId (E9).
func (c *Client) ToolExecutions(ctx context.Context, id string) (out Reconciliation, err error) {
	return out, c.Do(ctx, "GET", "/api/chats/"+esc(id)+"/tool_executions", nil, &out)
}

// Commands returns the chat's slash commands (built-in ones and pi's).
func (c *Client) Commands(ctx context.Context, id string) (out []Command, err error) {
	return out, c.Do(ctx, "GET", "/api/chats/"+esc(id)+"/commands", nil, &out)
}

// RunCommand runs a slash command, e.g. "/compact focus on code".
func (c *Client) RunCommand(ctx context.Context, id, command string) (out CommandResult, err error) {
	return out, c.Do(ctx, "POST", "/api/chats/"+esc(id)+"/commands", map[string]string{"command": command}, &out)
}

// Session writes pi's JSONL session file to w.
func (c *Client) Session(ctx context.Context, id string, w io.Writer) error {
	resp, err := c.request(ctx, "GET", "/api/chats/"+esc(id)+"/session", nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

func (c *Client) Artifacts(ctx context.Context, id string) (out []Artifact, err error) {
	return out, c.Do(ctx, "GET", "/api/chats/"+esc(id)+"/artifacts", nil, &out)
}

// Download writes the artifact to w; kind is "input" or "output" (empty = output).
func (c *Client) Download(ctx context.Context, id, name, kind string, w io.Writer) (int64, error) {
	p := "/api/chats/" + esc(id) + "/artifacts/" + esc(name)
	if kind != "" {
		p += "?kind=" + url.QueryEscape(kind)
	}
	resp, err := c.request(ctx, "GET", p, nil, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return io.Copy(w, resp.Body)
}

// Upload uploads the files as multipart (field "file", repeated). The body is streamed.
func (c *Client) Upload(ctx context.Context, id string, paths []string) (out []Artifact, err error) {
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return nil, err
		}
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			for _, p := range paths {
				f, err := os.Open(p)
				if err != nil {
					return err
				}
				part, err := mw.CreateFormFile("file", filepath.Base(p))
				if err == nil {
					_, err = io.Copy(part, f)
				}
				f.Close()
				if err != nil {
					return err
				}
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	resp, err := c.request(ctx, "POST", "/api/chats/"+esc(id)+"/files", pr, mw.FormDataContentType())
	pr.Close()
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("response to the upload not readable: %w", err)
	}
	return out, nil
}

// Approvals: state "" returns all, otherwise e.g. "pending".
func (c *Client) Approvals(ctx context.Context, state string) (out []Approval, err error) {
	p := "/api/approvals"
	if state != "" {
		p += "?state=" + url.QueryEscape(state)
	}
	return out, c.Do(ctx, "GET", p, nil, &out)
}

func (c *Client) Decide(ctx context.Context, approvalID string, approve bool) (out Approval, err error) {
	return out, c.Do(ctx, "POST", "/api/approvals/"+esc(approvalID), map[string]bool{"approve": approve}, &out)
}

// Events opens the SSE stream of a chat. Returns as soon as the server has sent the headers –
// from then on the subscription is active.
func (c *Client) Events(ctx context.Context, id string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/api/chats/"+esc(id)+"/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("orchestrator at %s not reachable: %w", c.BaseURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		return nil, &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	return resp.Body, nil
}
