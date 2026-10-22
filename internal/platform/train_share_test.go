// SPDX-FileCopyrightText: 2026 Philipp Schröer
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestArgsIntIntegralNumber(t *testing.T) {
	for _, s := range []string{"70.0", "7e1"} {
		n, ok, err := Args{"x": json.Number(s)}.int("x")
		if err != nil || !ok || n != 70 {
			t.Errorf("json.Number(%s): %d %v %v", s, n, ok, err)
		}
	}
	for _, s := range []string{"70.5", "1e30"} {
		if _, _, err := (Args{"x": json.Number(s)}).int("x"); err == nil || err.Error() != "x must be an integer" {
			t.Errorf("json.Number(%s): %v", s, err)
		}
	}
	if _, _, err := (Args{"x": float64(1e30)}).int("x"); err == nil {
		t.Error("float64(1e30) accepted")
	}
}

func TestSplitDatasetFractionRefused(t *testing.T) {
	split, ok := Lookup("split_dataset")
	if !ok {
		t.Fatal("split_dataset missing")
	}
	for _, raw := range []string{`{"dataset_id":12,"train_share":0.7}`, `{"dataset_id":12,"train_share":"0.7"}`} {
		_, err := split.Build(json.RawMessage(raw))
		if err == nil || err.Error() != MsgTrainShareFraction {
			t.Errorf("%s: %v", raw, err)
		}
	}
	req, err := split.Build(json.RawMessage(`{"dataset_id":12,"train_share":70.0}`))
	if err != nil || !strings.Contains(string(req.Body), `"train_share":70`) || strings.Contains(string(req.Body), `70.0`) {
		t.Errorf("70.0: %s %v", req.Body, err)
	}
}

func TestGatewaySplitFractionSameAsTool(t *testing.T) {
	tool, args, why := GatewayCall("POST", "/_agw/assess/splits", nil, []byte(`{"dataset_id":5,"train_share":0.7}`))
	if why != "" {
		t.Fatal(why)
	}
	_, errREST := tool.Build(args)
	split, _ := Lookup("split_dataset")
	_, errDirect := split.Build(json.RawMessage(`{"dataset_id":5,"train_share":0.7}`))
	if errREST == nil || errDirect == nil || errREST.Error() != errDirect.Error() || errREST.Error() != MsgTrainShareFraction {
		t.Errorf("REST %v, tool %v", errREST, errDirect)
	}
}

func TestIndexLineSplitNamesPercentage(t *testing.T) {
	for _, line := range strings.Split(GatewayIndex(), "\n") {
		if strings.Contains(line, "/_agw/assess/splits") {
			if !strings.Contains(line, "whole percentage") || !strings.Contains(line, "default 70") {
				t.Errorf("line: %q", line)
			}
			return
		}
	}
	t.Error("no line for /_agw/assess/splits")
}
