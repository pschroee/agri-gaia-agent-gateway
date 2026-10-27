// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"

	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// The inference runtime in experiment chats (#237). The experiments measure the delivery on the unchanged platform
// (the setup's runtime, AGW_INFERENCE_RUNTIME), so an experiment chat builds inference images with that runtime
// only. A call for another runtime is refused (not overridden: the agent would report a runtime that was not
// built), and the device tag intel-cpu does not reach the agent (expHideDeviceTags, capability edge).
func init() { registerGuard(expRuntimeGuard) }

// The refusals (E5); tests and the plan quote them. They carry no {{tool:...}} placeholder: guard messages also
// go back on the raw REST path, where no placeholder is replaced.
const (
	msgExpRuntimeBuild  = "experiment run: inference images use the runtime of this setup (%s) only; template %s is the %s template. Build with the %s template and leave out runtime"
	msgExpRuntimeImport = "experiment run: only the %s template of this setup may be imported, not the %s template; leave out runtime"
	msgExpRuntimeStep   = "experiment run: inference images use the runtime of this setup (%s) only, not %s; leave out runtime"
)

const (
	buildImagePath   = "/container-images/build"
	importTemplPath  = "/inference-container-templates/create/git"
	templateListPath = "/inference-container-templates"
)

// experimentProfile is chatGate for checks that must fail closed: a read error is an error, not a product chat
// (E4). A chat that does not exist is none either; it can hold no run.
func (m *Manager) experimentProfile(ctx context.Context, chatID string) (*store.Experiment, error) {
	m.mu.Lock()
	e, ok := m.expCache[chatID]
	m.mu.Unlock()
	if ok && time.Since(e.at) < planCacheTTL {
		return e.exp, nil
	}
	model, exp, created, err := m.st.ChatGate(ctx, chatID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("chat profile not readable: %w", err)
	}
	m.mu.Lock()
	if m.expCache == nil {
		m.expCache = map[string]expGateEntry{}
	}
	m.expCache[chatID] = expGateEntry{model: model, exp: exp, created: created, at: time.Now()}
	m.mu.Unlock()
	return exp, nil
}

// expRuntimeGuard refuses, in an experiment chat, the import of a template of another runtime and the build of an
// image from one (E2, E3). Product chats are not touched. An unreadable profile or template list fails closed.
func expRuntimeGuard(ctx context.Context, m *Manager, chatID string, req platform.Request, a delegation.Access) (string, error) {
	if req.Method != http.MethodPost || (req.Path != buildImagePath && req.Path != importTemplPath) {
		return "", nil
	}
	exp, err := m.experimentProfile(ctx, chatID)
	if err != nil || exp == nil {
		return "", err
	}
	want := platform.DefaultRuntime()
	if req.Path == importTemplPath {
		// By URL, whatever the ref: a raw REST import of another branch of a runtime's repository is refused too.
		if rt := otherRuntimeOfURL(firstOf(req.Form["git_url"]), want); rt != "" {
			return fmt.Sprintf(msgExpRuntimeImport, want, rt), nil
		}
		return "", nil // other sources: blockedCall decides
	}
	tid := ""
	for _, x := range a.Also {
		if x.Resource == delegation.ContainerTemplate {
			tid = x.ID
		}
	}
	if tid == "" {
		return "", nil // the platform refuses a build without a template
	}
	rts, err := m.templateRuntimes(ctx, chatID)
	if err != nil {
		return "", err
	}
	if rt := rts[tid]; rt != "" && rt != want {
		return fmt.Sprintf(msgExpRuntimeBuild, want, tid, rt, want), nil
	}
	return "", nil
}

// otherRuntimeOfURL is the runtime whose official repository is url when it is not want, "" otherwise.
func otherRuntimeOfURL(url, want string) string {
	for _, s := range platform.TemplateSources {
		if s.GitURL == url && s.Runtime != want {
			return s.Runtime
		}
	}
	return ""
}

func firstOf(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// templateRuntimes reads the installed templates (GET /inference-container-templates) and answers the runtime of
// each by id ("" for a template none of the official sources). It is a read of the gateway itself, logged as such
// (op exp_runtime_read) and only made in experiment chats that build. An unreadable list is an error.
func (m *Manager) templateRuntimes(ctx context.Context, chatID string) (map[string]string, error) {
	res, err := m.opt.Platform.Do(ctx, chatID, platform.Request{Method: http.MethodGet, Path: templateListPath,
		Query: map[string]string{"limit": "1000"}, Full: true})
	if err != nil {
		return nil, err
	}
	result := "ok"
	if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		result = "error"
	}
	m.LogCallBy(ctx, m.slotOf(chatID), chatID, "orchestrator", "exp_runtime_read", "GET "+templateListPath, result)
	if result == "error" {
		return nil, fmt.Errorf("template list not readable: %s", res.Message)
	}
	var list []struct {
		ID     json.Number `json:"id"`
		GitURL string      `json:"git_url"`
		GitRef string      `json:"git_ref"`
	}
	d := json.NewDecoder(bytes.NewReader([]byte(res.Body)))
	d.UseNumber()
	if err := d.Decode(&list); err != nil {
		return nil, fmt.Errorf("template list unreadable: %w", err)
	}
	out := make(map[string]string, len(list))
	for _, t := range list {
		out[t.ID.String()] = platform.TemplateRuntime(t.GitURL, t.GitRef)
	}
	return out, nil
}

// expRuntimeStepProblems checks the steps of a proposal in an experiment chat (E6): no step may name another
// runtime, import the template of another runtime or build from a literally named template of another runtime.
// A placeholder as template id is left to the guard when the step runs. An unreadable profile or template list
// reports nothing here; the guard holds anyway.
func (m *Manager) expRuntimeStepProblems(ctx context.Context, chatID string, steps []store.PlanStep) error {
	exp, err := m.experimentProfile(ctx, chatID)
	if err != nil || exp == nil {
		return nil
	}
	want := platform.DefaultRuntime()
	var problems plan.Problems
	var rts map[string]string
	loaded := false
	for _, st := range steps {
		if st.Method != http.MethodPost || (st.Path != buildImagePath && st.Path != importTemplPath) {
			continue
		}
		var args map[string]any
		_ = json.Unmarshal(st.Args, &args)
		if rt, ok := args["runtime"].(string); ok && rt != "" && rt != want {
			problems = append(problems, plan.Problem{Step: st.Idx, Msg: fmt.Sprintf(msgExpRuntimeStep, want, rt)})
			continue
		}
		if st.Path == importTemplPath {
			var form map[string][]string
			_ = json.Unmarshal(st.Form, &form)
			if rt := otherRuntimeOfURL(firstOf(form["git_url"]), want); rt != "" {
				problems = append(problems, plan.Problem{Step: st.Idx, Msg: fmt.Sprintf(msgExpRuntimeImport, want, rt)})
			}
			continue
		}
		tid := literalTemplateID(st.Body)
		if tid == "" {
			continue
		}
		if !loaded {
			loaded = true
			if rts, err = m.templateRuntimes(ctx, chatID); err != nil {
				slog.Warn("experiment runtime: templates not readable, plan not checked", "chat", chatID, "error", err)
			}
		}
		if rt := rts[tid]; rt != "" && rt != want {
			problems = append(problems, plan.Problem{Step: st.Idx, Msg: fmt.Sprintf(msgExpRuntimeBuild, want, tid, rt, want)})
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return problems
}

// literalTemplateID is container_template_id of a build body when it is a number (or a number as text); a
// placeholder or a missing field answers "".
func literalTemplateID(body json.RawMessage) string {
	var b struct {
		ID json.RawMessage `json:"container_template_id"`
	}
	if json.Unmarshal(body, &b) != nil || len(b.ID) == 0 {
		return ""
	}
	s := string(bytes.Trim(b.ID, `"`))
	if _, err := strconv.ParseUint(s, 10, 64); err != nil {
		return ""
	}
	return s
}

var edgeDevicePathRe = regexp.MustCompile(`^/edge-devices(/[^/]+)?$`)

// expHideDeviceTags makes the answer of GET /edge-devices and GET /edge-devices/<id> leave out the tag intel-cpu
// in an experiment chat (E7b): the agent then does not choose another runtime for the device. The digest runs
// after a digest the request already had; an answer that is no JSON stays as it is.
func expHideDeviceTags(req platform.Request) platform.Request {
	if req.Method != http.MethodGet || !edgeDevicePathRe.MatchString(req.Path) {
		return req
	}
	prev := req.Digest
	req.Digest = func(b []byte) ([]byte, error) {
		if prev != nil {
			var err error
			if b, err = prev(b); err != nil {
				return nil, err
			}
		}
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		var v any
		if d.Decode(&v) != nil {
			return b, nil
		}
		hideTag(v)
		out, err := json.Marshal(v)
		if err != nil {
			return b, nil
		}
		return out, nil
	}
	return req
}

// hideTag removes the tag intel-cpu from the "tags" of an object or of every object of an array.
func hideTag(v any) {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			hideTag(e)
		}
	case map[string]any:
		if tags, ok := x["tags"].([]any); ok {
			x["tags"] = slices.DeleteFunc(tags, func(t any) bool { s, _ := t.(string); return s == platform.IntelCPUTag })
		}
	}
}
