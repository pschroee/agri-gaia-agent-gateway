// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// stepsDoer records every call and answers from answers ("METHOD path"); other calls get 200 and {}.
type stepsDoer struct {
	calls   []string
	answers map[string]Result
}

func (s *stepsDoer) do(_ context.Context, r Request) (Result, error) {
	s.calls = append(s.calls, r.String()+"|"+string(r.Body))
	if res, ok := s.answers[r.Method+" "+r.Path]; ok {
		return res, nil
	}
	return Result{Status: "ok", HTTPStatus: 200, Body: "{}"}, nil
}

func TestRunWithoutSteps(t *testing.T) {
	tool, ok := Lookup("list_datasets")
	if !ok {
		t.Fatal("list_datasets not found")
	}
	s := &stepsDoer{}
	res, refused, err := tool.Run(context.Background(), json.RawMessage(`{"limit":2}`), s.do)
	if err != nil || refused != "" || res.Status != "ok" {
		t.Fatalf("res=%+v refused=%q err=%v", res, refused, err)
	}
	if len(s.calls) != 1 || !strings.HasPrefix(s.calls[0], "GET /datasets?") || !strings.Contains(s.calls[0], "limit=2") {
		t.Fatalf("calls = %v", s.calls)
	}

	tool, _ = Lookup("get_dataset")
	s = &stepsDoer{}
	res, refused, _ = tool.Run(context.Background(), json.RawMessage(`{}`), s.do)
	if res.Status != "error" || refused == "" || len(s.calls) != 0 {
		t.Fatalf("missing id: res=%+v refused=%q calls=%v", res, refused, s.calls)
	}
}

// probeTool reads /models/<id>/status first, refuses when the name is "locked", and then sends a PATCH.
var probeTool = Tool{
	Name:   "steps_probe",
	CLI:    "steps-probe",
	Desc:   "test tool with steps",
	Params: []Param{{Name: "model_id", Type: "integer", Desc: "id", Required: true}},
	build:  byID(http.MethodGet, "/models", "model_id", "/status"),
	steps: func(ctx context.Context, a Args, final Request, do Doer) (Result, string, error) {
		id, _, err := a.int("model_id")
		if err != nil {
			return Result{Status: "error", Message: err.Error()}, err.Error(), nil
		}
		var st struct {
			Name string `json:"name"`
		}
		if res, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: "/models/" + strconv.FormatInt(id, 10)}, &st); stop {
			return res, "", err
		}
		if st.Name == "locked" {
			return Refuse("model %d is locked", id)
		}
		res, err := do(ctx, Request{Method: http.MethodPatch, Path: "/models/" + strconv.FormatInt(id, 10), Body: json.RawMessage(`{"name":"x"}`)})
		return res, "", err
	},
}

func TestRunWithSteps(t *testing.T) {
	s := &stepsDoer{answers: map[string]Result{
		"GET /models/5": {Status: "ok", HTTPStatus: 200, Body: `{"name":"free"}`},
	}}
	res, refused, err := probeTool.Run(context.Background(), json.RawMessage(`{"model_id":5}`), s.do)
	if err != nil || refused != "" || res.Status != "ok" {
		t.Fatalf("res=%+v refused=%q err=%v", res, refused, err)
	}
	if len(s.calls) != 2 || !strings.HasPrefix(s.calls[0], "GET /models/5") || !strings.HasPrefix(s.calls[1], "PATCH /models/5") {
		t.Fatalf("calls in wrong order: %v", s.calls)
	}

	s = &stepsDoer{answers: map[string]Result{
		"GET /models/5": {Status: "ok", HTTPStatus: 200, Body: `{"name":"locked"}`},
	}}
	res, refused, _ = probeTool.Run(context.Background(), json.RawMessage(`{"model_id":5}`), s.do)
	if refused != "model 5 is locked" || res.Status != "error" || len(s.calls) != 1 {
		t.Fatalf("locked: res=%+v refused=%q calls=%v", res, refused, s.calls)
	}

	s = &stepsDoer{}
	_, refused, _ = probeTool.Run(context.Background(), json.RawMessage(`{"model_id":"x"}`), s.do)
	if refused == "" || len(s.calls) != 0 {
		t.Fatalf("invalid argument: refused=%q calls=%v", refused, s.calls)
	}
}

func TestReadJSONStops(t *testing.T) {
	cases := map[string]Result{
		"denied":    {Status: "denied", Message: "no"},
		"error 404": {Status: "ok", HTTPStatus: 404, Body: `{}`},
		"not JSON":  {Status: "ok", HTTPStatus: 200, Body: `<html>`},
	}
	for name, answer := range cases {
		s := &stepsDoer{answers: map[string]Result{"GET /x": answer}}
		var v map[string]any
		res, stop, _ := ReadJSON(context.Background(), s.do, Request{Method: http.MethodGet, Path: "/x"}, &v)
		if !stop || res.Status == "ok" {
			t.Errorf("%s: stop=%v res=%+v", name, stop, res)
		}
	}
}
