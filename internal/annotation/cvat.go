// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package annotation is the pre-annotation service of the gateway (V1): it prepares a dataset in CVAT,
// runs a pre-annotation function over it, follows the run, counts the result and writes the annotations
// back into the dataset. It talks to CVAT with the user's own session (E1), which never leaves this
// package (E2), and reaches the platform only through platform.Client.DoTrusted (E5).
package annotation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound: CVAT does not know the object. A run of the rq queue answers this once the queue has
// dropped it; a task answers it when somebody deleted it in CVAT.
var ErrNotFound = errors.New("not found in CVAT")

// CVAT is the client of the CVAT server of the platform's instance. The API below /api needs the
// session of the user, which the gateway gets from the platform (POST /cvat/auth/login) and holds in
// memory (E2).
type CVAT struct {
	BaseURL string
	HTTP    *http.Client
}

// Limits of one answer of the CVAT server; every call sets the one that fits.
const (
	maxFunctionsBytes = 4 << 20
	maxTaskBytes      = 4 << 20
	maxRunBytes       = 1 << 20
	maxAnnotations    = 64 << 20
	maxLabelsBytes    = 4 << 20
)

// NewCVAT builds the client of a CVAT server. An empty url gives nil (the service is then not wired).
func NewCVAT(baseURL string, hc *http.Client) (*CVAT, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, nil
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("AGW_CVAT_API_URL invalid: %q", baseURL)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	c := *hc
	// Neither the session nor a query goes to a foreign host on a redirect.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &CVAT{BaseURL: baseURL, HTTP: &c}, nil
}

// API is the API path of the CVAT server.
const cvatAPI = "/api"

// Function is a pre-annotation function as CVAT lists it (a Nuclio detector).
type Function struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Kind   string   `json:"kind"`
	Labels []string `json:"labels"`
}

// Label is a class of a CVAT task.
type Label struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Owner names the user a CVAT object belongs to.
type Owner struct {
	Username string `json:"username"`
}

// Task is the part of a CVAT task the service needs.
type Task struct {
	ID     int64     `json:"id"`
	Name   string    `json:"name"`
	Size   int       `json:"size"`
	Owner  Owner     `json:"owner"`
	Labels labelList `json:"labels"`
}

// labelList is the "labels" field of a task. CVAT 2.4.2 answers a page of the label list there
// ({"url": ..., "count": n}) instead of the labels themselves, and reading that object into a list
// makes the whole task unreadable - which is what broke annotation-summary on the instance on
// 16.10.2026. Both forms are accepted: with a page the list stays empty, and Labels() then fetches
// the classes from GET /api/labels.
type labelList []Label

func (l *labelList) UnmarshalJSON(raw []byte) error {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' {
		var arr []Label
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return err
		}
		*l = arr
		return nil
	}
	*l = nil // a page or null: no class in the task itself
	return nil
}

// Annotations is the annotation of a task in CVAT's JSON form.
type Annotations struct {
	Tags   []Tag   `json:"tags"`
	Shapes []Shape `json:"shapes"`
	Tracks []Track `json:"tracks"`
}

// Tag is one classification annotation of a frame.
type Tag struct {
	Frame   int    `json:"frame"`
	LabelID int64  `json:"label_id"`
	Source  string `json:"source"`
}

// Shape is one object annotation of a frame. Points is the box [x1,y1,x2,y2] of a rectangle; V3 reads
// it to compare the manual annotation of the user with what a function answers.
type Shape struct {
	Frame   int       `json:"frame"`
	LabelID int64     `json:"label_id"`
	Type    string    `json:"type"`
	Source  string    `json:"source"`
	Outside bool      `json:"outside"`
	Points  []float64 `json:"points"`
}

// Track is a sequence of shapes of one object over several frames.
type Track struct {
	LabelID int64   `json:"label_id"`
	Source  string  `json:"source"`
	Shapes  []Shape `json:"shapes"`
}

// Run is one request of the CVAT queue (an rq job). Status and progress are rq's; progress is percent.
type Run struct {
	ID       string  `json:"id"`
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	Started  string  `json:"started"`
	Ended    string  `json:"ended"`
	ExcInfo  string  `json:"exc_info"`
}

// RunRequest is the body of POST /api/lambda/requests: which function runs over which task.
type RunRequest struct {
	Function  string           `json:"function"`
	Task      int64            `json:"task"`
	Quality   string           `json:"quality"`
	Cleanup   bool             `json:"cleanup"`
	Threshold float64          `json:"threshold"`
	Mapping   map[string]Descr `json:"mapping"`
}

// Descr is CVAT's form of a mapped class: the name of the class in the task.
type Descr struct {
	Name string `json:"name"`
}

// Functions lists the functions CVAT knows (every user sees them; the service filters them, E7).
func (c *CVAT) Functions(ctx context.Context, s session) ([]Function, error) {
	raw, err := c.do(ctx, s, http.MethodGet, cvatAPI+"/lambda/functions", nil, nil, maxFunctionsBytes)
	if err != nil {
		return nil, err
	}
	var out []Function
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errors.New("CVAT's function list is not readable")
	}
	return out, nil
}

// Function reads one function.
func (c *CVAT) Function(ctx context.Context, s session, id string) (Function, error) {
	raw, err := c.do(ctx, s, http.MethodGet, cvatAPI+"/lambda/functions/"+url.PathEscape(id), nil, nil, maxFunctionsBytes)
	if err != nil {
		return Function{}, err
	}
	var out Function
	if err := json.Unmarshal(raw, &out); err != nil {
		return Function{}, errors.New("CVAT's function is not readable")
	}
	return out, nil
}

// Task reads a task; NotFound means the task is gone in CVAT.
func (c *CVAT) Task(ctx context.Context, s session, id int64) (Task, error) {
	raw, err := c.do(ctx, s, http.MethodGet, cvatAPI+"/tasks/"+strconv.FormatInt(id, 10), nil, nil, maxTaskBytes)
	if err != nil {
		return Task{}, err
	}
	var t Task
	if err := json.Unmarshal(raw, &t); err != nil {
		return Task{}, errors.New("CVAT's task is not readable")
	}
	return t, nil
}

// TaskAnnotations reads the annotations of a task.
func (c *CVAT) TaskAnnotations(ctx context.Context, s session, id int64) (Annotations, error) {
	raw, err := c.do(ctx, s, http.MethodGet, cvatAPI+"/tasks/"+strconv.FormatInt(id, 10)+"/annotations", nil, nil, maxAnnotations)
	if err != nil {
		return Annotations{}, err
	}
	var a Annotations
	if err := json.Unmarshal(raw, &a); err != nil {
		return Annotations{}, errors.New("CVAT's annotations are not readable")
	}
	return a, nil
}

// Labels reads the classes of a task: from the task itself, otherwise from the label list (the field
// is part of the task detail of CVAT 2.4.2, but not of every answer).
func (c *CVAT) Labels(ctx context.Context, s session, taskID int64) ([]Label, error) {
	t, err := c.Task(ctx, s, taskID)
	if err == nil && len(t.Labels) > 0 {
		return t.Labels, nil
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	q := url.Values{"task_id": {strconv.FormatInt(taskID, 10)}, "page_size": {"500"}}
	raw, err := c.do(ctx, s, http.MethodGet, cvatAPI+"/labels", q, nil, maxLabelsBytes)
	if err != nil {
		return nil, err
	}
	var page struct {
		Results []Label `json:"results"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, errors.New("CVAT's label list is not readable")
	}
	return page.Results, nil
}

// CreateRequest starts a run over a task and returns its request.
func (c *CVAT) CreateRequest(ctx context.Context, s session, r RunRequest) (Run, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return Run{}, err
	}
	raw, err := c.do(ctx, s, http.MethodPost, cvatAPI+"/lambda/requests", nil, body, maxRunBytes)
	if err != nil {
		return Run{}, err
	}
	var out Run
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
		return Run{}, errors.New("CVAT did not answer the run with a request id")
	}
	return out, nil
}

// Request reads the state of a run; NotFound means the queue has dropped it (the annotations stay in
// the task).
func (c *CVAT) Request(ctx context.Context, s session, id string) (Run, error) {
	raw, err := c.do(ctx, s, http.MethodGet, cvatAPI+"/lambda/requests/"+url.PathEscape(id), nil, nil, maxRunBytes)
	if err != nil {
		return Run{}, err
	}
	var out Run
	if err := json.Unmarshal(raw, &out); err != nil {
		return Run{}, errors.New("CVAT's run is not readable")
	}
	return out, nil
}

// do performs one call with the user's session. The session goes into headers and cookie and never
// into an error or a message.
func (c *CVAT) do(ctx context.Context, s session, method, path string, query url.Values, body []byte, limit int64) ([]byte, error) {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+s.key)
	// CVAT does not serve application/json: its renderers answer only to their own media type
	// (application/vnd.cvat+json), and a request that asks for application/json alone is refused
	// with 406 "Could not satisfy the request Accept header" before authentication is looked at.
	// Asking for both keeps JSON as the preference and still lets CVAT pick its own type.
	req.Header.Set("Accept", "application/json, */*")
	if s.csrf != "" {
		req.Header.Set("X-CSRFToken", s.csrf)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.sessionID != "" {
		req.AddCookie(&http.Cookie{Name: "sessionid", Value: s.sessionID})
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, errCVAT{method: method, path: path, err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, errCVAT{method: method, path: path, status: resp.StatusCode, err: err}
	}
	if int64(len(raw)) > limit {
		return nil, errCVAT{method: method, path: path, status: resp.StatusCode,
			err: fmt.Errorf("the answer is larger than %d MiB", limit>>20)}
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return raw, errCVAT{method: method, path: path, status: resp.StatusCode, err: ErrNotFound}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return raw, errCVAT{method: method, path: path, status: resp.StatusCode,
			err: fmt.Errorf("CVAT answers HTTP %d", resp.StatusCode)}
	}
	return raw, nil
}
