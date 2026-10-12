// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// Doer executes one call of a tool with steps. In the socket it is the same path as a single call
// (delegation, approval, log), so every step is checked and logged on its own.
type Doer func(ctx context.Context, r Request) (Result, error)

// Steps runs a tool as a sequence of calls, e.g. reads before the call that build describes. It gets
// the checked arguments, the built call and do. refused is the reason for the log when the tool stops
// on its own before its last call (empty otherwise).
type Steps func(ctx context.Context, a Args, final Request, do Doer) (res Result, refused string, err error)

// HasSteps reports whether the tool runs more than one call.
func (t Tool) HasSteps() bool { return t.steps != nil }

// Run checks the arguments, builds the call and executes it through do; a tool with steps runs its
// steps instead. refused is set when the tool refused before any call went out.
func (t Tool) Run(ctx context.Context, raw json.RawMessage, do Doer) (res Result, refused string, err error) {
	final, err := t.Build(raw)
	if err != nil {
		return Result{Status: "error", Message: err.Error()}, err.Error(), nil
	}
	if t.steps == nil {
		res, err := do(ctx, final)
		return res, "", err
	}
	a := Args{}
	if b := bytes.TrimSpace(raw); len(b) > 0 && string(b) != "null" {
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		if err := d.Decode(&a); err != nil {
			return Result{Status: "error", Message: err.Error()}, err.Error(), nil
		}
	}
	return t.steps(ctx, a, final, do)
}

// Refuse ends a tool with steps before its last call; the message tells the agent why.
func Refuse(format string, args ...any) (Result, string, error) {
	msg := fmt.Sprintf(format, args...)
	return Result{Status: "error", Message: msg}, msg, nil
}

// ReadJSON runs a reading call through do and decodes its JSON body into v (numbers as json.Number).
// stop is true when the call did not succeed with 2xx or the body is not the expected JSON; res is then
// what the agent gets.
func ReadJSON(ctx context.Context, do Doer, r Request, v any) (res Result, stop bool, err error) {
	r.Full = true
	res, err = do(ctx, r)
	if err != nil {
		return res, true, err
	}
	if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return res, true, nil
	}
	d := json.NewDecoder(bytes.NewReader([]byte(res.Body)))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return Result{Status: "error", Message: fmt.Sprintf("%s: response is not the expected JSON: %v", r.String(), err)}, true, nil
	}
	return res, false, nil
}
