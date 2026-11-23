// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package exp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

type fakeCheckClient struct {
	answers  map[string]string
	errs     map[string]error
	asked    []string
	lastText string
	lastErr  error
	calls    []LoggedCall
	callsErr error

	textAsked bool
}

func (f *fakeCheckClient) Get(_ context.Context, path string) (json.RawMessage, error) {
	f.asked = append(f.asked, path)
	if err := f.errs[path]; err != nil {
		return nil, err
	}
	return json.RawMessage(f.answers[path]), nil
}

func (f *fakeCheckClient) LastAssistantText(context.Context) (string, error) {
	f.textAsked = true
	return f.lastText, f.lastErr
}

func (f *fakeCheckClient) PlatformCalls(context.Context) ([]LoggedCall, error) {
	return f.calls, f.callsErr
}

func intp(n int) *int { return &n }

func answerSpec(a AnswerCheck) CheckSpec { return CheckSpec{Answer: &a} }

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
		{"contains string", Expectation{Path: "$.datasets[0].name", Op: "contains", Value: "nis"}, true},
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
			if f.textAsked {
				t.Fatal("a check without answer must not read the chat")
			}
		})
	}
}

func TestEvaluateUsesCreatedObjects(t *testing.T) {
	f := &fakeCheckClient{answers: map[string]string{"/datasets": `{"datasets":[{"id":"1"}]}`}}
	cs := spec("/datasets", Expectation{Path: "$.datasets[*].id", Op: "exists"})
	cs.Created = &Created{Resource: "dataset", Count: intp(1)}
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

	cs.Created.Count = intp(2)
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

func TestAnswerCheckMatchesTextAndNumber(t *testing.T) {
	three := int64(3)
	f := &fakeCheckClient{lastText: "I found 3 datasets: mnist, test and demo."}
	pass, evidence, err := Evaluate(context.Background(), f,
		answerSpec(AnswerCheck{Contains: "3 datasets", Count: &three}), nil)
	if err != nil || !pass {
		t.Fatalf("pass %v, err %v, evidence %s", pass, err, evidence)
	}
	var ev Evidence
	if err := json.Unmarshal(evidence, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Answer == nil || !ev.Answer.OK || !ev.Answer.ContainsOK || !ev.Answer.CountOK {
		t.Fatalf("answer evidence: %+v", ev.Answer)
	}
	if ev.Answer.Text != f.lastText {
		t.Fatalf("evidence text %q, want %q", ev.Answer.Text, f.lastText)
	}
}

func TestAnswerCheckFails(t *testing.T) {
	thirteen := int64(13)
	f := &fakeCheckClient{lastText: "I found 3 datasets."}
	pass, evidence, err := Evaluate(context.Background(), f,
		answerSpec(AnswerCheck{Contains: "4 datasets", Count: &thirteen}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if pass {
		t.Fatalf("a different number and text must not pass: %s", evidence)
	}
	var ev Evidence
	if err := json.Unmarshal(evidence, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Answer == nil || ev.Answer.OK || ev.Answer.ContainsOK || ev.Answer.CountOK {
		t.Fatalf("answer evidence: %+v", ev.Answer)
	}
}

func TestAnswerCheckEmptyAnswer(t *testing.T) {
	f := &fakeCheckClient{}
	pass, evidence, err := Evaluate(context.Background(), f,
		answerSpec(AnswerCheck{Contains: "dataset"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if pass {
		t.Fatalf("an empty answer must not pass: %s", evidence)
	}
	if !strings.Contains(string(evidence), `"text":""`) {
		t.Fatalf("evidence should show the empty answer: %s", evidence)
	}
}

func TestAnswerCheckIgnoresToolOutputAndSystemNotes(t *testing.T) {
	messages := []Message{
		{Role: "user", Message: json.RawMessage(`{"role":"user","content":[{"type":"text","text":"How many datasets?"}]}`)},
		{Role: "assistant", Message: json.RawMessage(`{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"bash"}]}`)},
		{Role: "toolResult", Message: json.RawMessage(`{"role":"toolResult","toolCallId":"c1","content":[{"type":"text","text":"3 datasets"}]}`)},
		{Role: "assistant", Message: json.RawMessage(`{"role":"assistant","content":[{"type":"thinking","thinking":"3 datasets"},{"type":"text","text":"I could not count them."}]}`)},
		{Role: "system", Message: json.RawMessage(`{"role":"system","content":[{"type":"text","text":"3 datasets"}]}`)},
	}
	text := AssistantText(messages)
	if text != "I could not count them." {
		t.Fatalf("last assistant text %q", text)
	}
	f := &fakeCheckClient{lastText: text}
	pass, evidence, err := Evaluate(context.Background(), f,
		answerSpec(AnswerCheck{Contains: "3 datasets"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if pass {
		t.Fatalf("a string only in a tool output or system note must not pass: %s", evidence)
	}
}

func TestAssistantTextJoinsTextItems(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Message: json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"first"},{"type":"toolCall","name":"bash"},{"type":"text","text":"second"}]}`)},
	}
	if got := AssistantText(messages); got != "first\nsecond" {
		t.Fatalf("text %q", got)
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

// TestAnswerCountVectors holds the answer number rule (BM17 K1) against the shared vectors; the
// thesis repo's abgleich_online.py reads the same file.
func TestAnswerCountVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/answer_count_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Description string `json:"description"`
		Version     int    `json:"version"`
		Vectors     []struct {
			Name   string `json:"name"`
			Text   string `json:"text"`
			Want   int64  `json:"want"`
			Number string `json:"number"`
			OK     bool   `json:"ok"`
		} `json:"vectors"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 || len(doc.Vectors) == 0 {
		t.Fatalf("version %d, %d vectors", doc.Version, len(doc.Vectors))
	}
	for _, v := range doc.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			if got := containsNumber(v.Text, v.Want); got != v.OK {
				t.Errorf("containsNumber(%q, %d) = %v, want %v", v.Text, v.Want, got, v.OK)
			}
			if tok, _ := answerNumber(v.Text); tok != v.Number {
				t.Errorf("answerNumber(%q) = %q, want %q", v.Text, tok, v.Number)
			}
		})
	}
}

func TestAnswerCountEvidenceNamesTheNumber(t *testing.T) {
	want := int64(22)
	f := &fakeCheckClient{lastText: "23 datasets, see\n22. [garage-notizen] and 1.22 GB"}
	pass, evidence, err := Evaluate(context.Background(), f, answerSpec(AnswerCheck{Count: &want}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if pass {
		t.Fatalf("23 as the answer must not pass for 22: %s", evidence)
	}
	var ev Evidence
	if err := json.Unmarshal(evidence, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Answer == nil || ev.Answer.Number != "23" || ev.Answer.CountOK {
		t.Fatalf("answer evidence: %+v", ev.Answer)
	}
}

// BM17 K2: the number the answer must carry is counted from the list the agent reads, when the check runs.
func countFromSpec() CheckSpec {
	return answerSpec(AnswerCheck{CountFrom: &CountFrom{Path: "/datasets", Select: "$[*].name", Prefix: "garage"}})
}

const datasetsAnswer = `[{"name":"mnist-a"},{"name":"garage-clips-train-teil1"},{"name":"garage-notizen"},` +
	`{"name":"a-garage-x"},{"name":"garage-auto-train"}]`

func TestAnswerCountFromDerivesTheNumberFromTheList(t *testing.T) {
	f := &fakeCheckClient{answers: map[string]string{"/datasets": datasetsAnswer}, lastText: "3 datasets start with garage."}
	pass, evidence, err := Evaluate(context.Background(), f, countFromSpec(), nil)
	if err != nil || !pass {
		t.Fatalf("pass %v, err %v, evidence %s", pass, err, evidence)
	}
	var ev Evidence
	if err := json.Unmarshal(evidence, &ev); err != nil {
		t.Fatal(err)
	}
	a := ev.Answer
	if a == nil || a.Count == nil || *a.Count != 3 || a.CountFrom == nil || len(a.CountFrom.Names) != 3 || a.Number != "3" {
		t.Fatalf("answer evidence: %s", evidence)
	}
}

func TestAnswerCountFromFollowsTheState(t *testing.T) {
	// A leftover with the prefix moves the required number with the list the agent sees: 3 no longer passes.
	f := &fakeCheckClient{answers: map[string]string{"/datasets": strings.Replace(datasetsAnswer, `{"name":"mnist-a"}`, `{"name":"garage-rest"}`, 1)}, lastText: "3 datasets."}
	pass, evidence, err := Evaluate(context.Background(), f, countFromSpec(), nil)
	if err != nil || pass {
		t.Fatalf("3 must fail against 4 names: pass %v, err %v, %s", pass, err, evidence)
	}
	f.lastText = "4 datasets."
	if pass, evidence, err = Evaluate(context.Background(), f, countFromSpec(), nil); err != nil || !pass {
		t.Fatalf("4 must pass: pass %v, err %v, %s", pass, err, evidence)
	}
}

func TestAnswerCountFromFailsWithoutTheList(t *testing.T) {
	f := &fakeCheckClient{errs: map[string]error{"/datasets": errors.New("503")}, lastText: "3 datasets."}
	pass, evidence, err := Evaluate(context.Background(), f, countFromSpec(), nil)
	if err == nil || pass {
		t.Fatalf("an unreadable list must fail the check: pass %v, err %v", pass, err)
	}
	var ev Evidence
	if jerr := json.Unmarshal(evidence, &ev); jerr != nil || ev.Answer == nil || ev.Answer.CountFrom == nil || ev.Answer.CountFrom.Error == "" {
		t.Fatalf("evidence keeps the failure: %s", evidence)
	}
}
