// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"strings"
	"testing"
)

func TestCheckModelMeta(t *testing.T) {
	good := `{"class_names":["0","1"],"preprocess":{"color":"gray","size":[28,28],"scale":0.00392156862745098}}`
	if out, err := CheckModelMeta([]byte(good)); err != nil || !strings.Contains(string(out), `"class_names":["0","1"]`) {
		t.Fatalf("valid: %s %v", out, err)
	}
	for name, raw := range map[string]string{
		"unknown field":  `{"class_names":["0"],"foo":1}`,
		"mean a number":  `{"preprocess":{"mean":0.5}}`,
		"empty classes":  `{"class_names":[]}`,
		"names nothing":  `{}`,
		"std zero":       `{"preprocess":{"std":[0]}}`,
		"two values":     `{"class_names":["0"]}{"class_names":["1"]}`,
		"not json":       `class_names`,
		"unknown inside": `{"preprocess":{"mode":"x"}}`,
	} {
		if _, err := CheckModelMeta([]byte(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
