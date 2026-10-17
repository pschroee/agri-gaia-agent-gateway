// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package exp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeCheckClient struct {
	answers map[string]string
	errs    map[string]error
	asked   []string
}

func (f *fakeCheckClient) Get(_ context.Context, path string) (json.RawMessage, error) {
	f.asked = append(f.asked, path)
	if err := f.errs[path]; err != nil {
		return nil, err
	}
	return json.RawMessage(f.answers[path]), nil
}

func spec(path string, expect ...Expectation) CheckSpec {
	return CheckSpec{Conditions: []Condition{{Request: Request{Method: "GET", Path: path}, Expect: expect}}}
}

func TestEvaluateOps(t *testing.T) {
	answer := `{"datasets":[{"id":"1","name":"mnist","public":true},{"id":"2","name":"test","public":false}],"count":2}`
	cases := []struct {
		name string
		e    Expectation
		pass bool
	}{
		{"eq string", Expectation{Path: "$.datasets[0].name", Op: "eq", Value: "mnist"}, true},
		{"eq number", Expectation{Path: "$.count", Op: "eq", Value: 2}, true},
		{"eq missing", Expectation{Path: "$.datasets[5].name", Op: "eq", Value: "mnist"}, false},
		{"exists wildcard", Expectation{Path: "$.datasets[*].id", Op: "exists"}, true},
		{"exists missing", Expectation{Path: "$.models[*].id", Op: "exists"}, false},
		{"count", Expectation{Path: "$.datasets[*].id", Op: "count", Value: 2}, true},
		{"count wrong", Expectation{Path: "$.datasets[*].id", Op: "count", Value: 3}, false},
		{"contains string", Expectation{Path: "$.datasets[0].name", Op: "contains", Value: "nni"}, true},
		{"contains array value", Expectation{Path: "$.datasets[*].name", Op: "contains", Value: "test"}, true},
		{"contains array missing", Expectation{Path: "$.datasets[*].name", Op: "contains", Value: "dogs"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeCheckClient{answers: map[string]string{"/datasets": answer}}
			pass, evidence, err := Evaluate(context.Background(), f, spec("/datasets", c.e), nil)
			if err != nil {
				t.Fatal(err)
			}
			if pass != c.pass {
				t.Fatalf("pass %v, want %v (evidence %s)", pass, c.pass, evidence)
			}
			var ev Evidence
			if err := json.Unmarshal(evidence, &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Pass != c.pass || len(ev.Conditions) != 1 || len(ev.Conditions[0].Result) != 1 {
				t.Fatalf("evidence: %+v", ev)
			}
		})
	}
}

func TestEvaluateUsesCreatedObjects(t *testing.T) {
	f := &fakeCheckClient{answers: map[string]string{"/datasets": `{"datasets":[{"id":"1"}]}`}}
	cs := spec("/datasets", Expectation{Path: "$.datasets[*].id", Op: "exists"})
	cs.Created = &Created{Resource: "dataset", Count: 1}
	created := []Object{{Resource: "dataset", ID: "1"}, {Resource: "model", ID: "9"}}
	pass, evidence, err := Evaluate(context.Background(), f, cs, created)
	if err != nil || !pass {
		t.Fatalf("pass %v, err %v, evidence %s", pass, err, evidence)
	}
	var ev Evidence
	if err := json.Unmarshal(evidence, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Created == nil || !ev.Created.OK || ev.Created.Got != 1 {
		t.Fatalf("created evidence: %+v", ev.Created)
	}

	cs.Created.Count = 2
	pass, evidence, err = Evaluate(context.Background(), f, cs, created)
	if err != nil || pass {
		t.Fatalf("two datasets expected: pass %v, err %v, evidence %s", pass, err, evidence)
	}
	if !strings.Contains(string(evidence), `"got":1`) {
		t.Fatalf("evidence should name the count: %s", evidence)
	}
}

func TestEvaluateKeepsFailedCallInEvidence(t *testing.T) {
	f := &fakeCheckClient{errs: map[string]error{"/datasets": errors.New("platform responds 503")}}
	pass, evidence, err := Evaluate(context.Background(), f, spec("/datasets", Expectation{Path: "$[*]", Op: "exists"}), nil)
	if pass || err == nil {
		t.Fatalf("pass %v, err %v", pass, err)
	}
	var ev Evidence
	if err := json.Unmarshal(evidence, &ev); err != nil {
		t.Fatal(err)
	}
	if len(ev.Conditions) != 1 || !strings.Contains(ev.Conditions[0].Error, "503") {
		t.Fatalf("evidence: %+v", ev)
	}
}

func TestEvaluateRefusesNonJSONAndEmptyCheck(t *testing.T) {
	f := &fakeCheckClient{answers: map[string]string{"/datasets": "<html>no</html>"}}
	pass, _, err := Evaluate(context.Background(), f, spec("/datasets", Expectation{Path: "$", Op: "exists"}), nil)
	if pass || err == nil {
		t.Fatalf("non-JSON: pass %v, err %v", pass, err)
	}
	pass, _, err = Evaluate(context.Background(), f, CheckSpec{}, nil)
	if pass || err == nil {
		t.Fatalf("empty check: pass %v, err %v", pass, err)
	}
}

func TestParseJSONPath(t *testing.T) {
	good := []string{"$", "$.a", "$.a.b", "$[*]", "$[0]", "$.datasets[*].id", "$.a[2].b[*].c"}
	for _, p := range good {
		if _, err := parseJSONPath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"a", "$.", "$[x]", "$[-1]", "$..a"} {
		if _, err := parseJSONPath(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}
