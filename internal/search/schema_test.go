// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import (
	"encoding/json"
	"testing"
)

const testSchema = `{"properties":{
  "lr":{"type":"number","minimum":0,"maximum":1},
  "batch_size":{"type":"integer","minimum":1,"maximum":128},
  "optimizer":{"type":"string","enum":["adam","sgd","rmsprop"]},
  "pretrained":{"type":"boolean"},
  "epochs":{"type":"integer"}}}`

func TestCheckSchema(t *testing.T) {
	ok := mustSpace(t, `{"lr":{"type":"float","min":0.0001,"max":0.1,"scale":"log"},"batch_size":{"type":"int","min":8,"max":64},
	  "optimizer":{"type":"choice","values":["adam","sgd"]},"pretrained":{"type":"bool"}}`)
	if err := CheckSchema(ok, json.RawMessage(testSchema)); err != nil {
		t.Fatalf("a space inside the schema refused: %v", err)
	}
	bad := map[string]string{
		"unknown setting":   `{"momentum":{"type":"float","min":0,"max":1}}`,
		"bound above":       `{"lr":{"type":"float","min":0.1,"max":2}}`,
		"int bound above":   `{"batch_size":{"type":"int","min":8,"max":256}}`,
		"float for integer": `{"epochs":{"type":"float","min":1,"max":5}}`,
		"enum value":        `{"optimizer":{"type":"choice","values":["adam","lion"]}}`,
		"choice type":       `{"optimizer":{"type":"choice","values":[1,2]}}`,
		"bool for number":   `{"lr":{"type":"bool"}}`,
	}
	for name, raw := range bad {
		if err := CheckSchema(mustSpace(t, raw), json.RawMessage(testSchema)); err == nil {
			t.Errorf("%s: accepted %s", name, raw)
		}
	}
	if err := CheckSchema(ok, json.RawMessage(`{}`)); err == nil {
		t.Error("a schema without properties accepted")
	}
}
