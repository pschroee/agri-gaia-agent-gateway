// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import "testing"

func TestCanonicalSortsKeysAndKeepsNumbers(t *testing.T) {
	got, err := Canonical([]byte(`{"b":[1,2.50,{"y":true,"x":null}],"a":"<&>","c":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":"<&>","b":[1,2.50,{"x":null,"y":true}],"c":9007199254740993}`
	if string(got) != want {
		t.Fatalf("canonical:\n got %s\nwant %s", got, want)
	}
}

func TestCanonicalRejectsAmbiguousJSON(t *testing.T) {
	for _, raw := range []string{`{"a":1,"a":2}`, `{} x`, "\"\xff\"", `{"a":`} {
		if _, err := Canonical([]byte(raw)); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	// Keys differing in case are two keys, not a duplicate.
	got, err := Canonical([]byte(`{"A":1,"a":2}`))
	if err != nil || string(got) != `{"A":1,"a":2}` {
		t.Fatalf("case-different keys: %s %v", got, err)
	}
}
