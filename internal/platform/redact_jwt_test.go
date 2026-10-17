// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// testJWT has the shape of a real access token: a JOSE header in base64url (it begins with "eyJ"), a
// payload and a signature, joined by dots.
const testJWT = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhZ2VudC10ZXN0IiwiZXhwIjoxNzkwMDAwMDAwfQ.c2lnbmF0dXJlLXBhcnQ"

// Issue #141: a MinIO AssumeRole failure carried a whole token in the free-text message of GET /tasks/<id>.
// The mark must appear in the plain text, in a JSON field and in a nested message.
func TestDoRedactsTokenInPlainText(t *testing.T) {
	f := &fakePlatform{respond: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "S3 server returned an error: AssumeRole failed for %s (status 500)", testJWT)
	}}
	c, _ := newClient(t, f, 0)
	res, err := c.Do(context.Background(), "", Request{Method: "GET", Path: "/tasks/7"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Body, testJWT) {
		t.Fatalf("the token reached the agent: %s", res.Body)
	}
	if !strings.Contains(res.Body, redacted) || !strings.Contains(res.Body, "AssumeRole failed for") {
		t.Fatalf("the mark is missing or too much was redacted: %s", res.Body)
	}
}

func TestDoRedactsTokenInJSONField(t *testing.T) {
	f := &fakePlatform{respond: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"failed","id":7,"message":"S3 assume role failed: %s"}`, testJWT)
	}}
	c, _ := newClient(t, f, 0)
	res, err := c.Do(context.Background(), "", Request{Method: "GET", Path: "/tasks/7"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Body, testJWT) {
		t.Fatalf("the token reached the agent: %s", res.Body)
	}
	for _, want := range []string{`"message":"S3 assume role failed: ` + redacted + `"`, `"id":7`, `"status":"failed"`} {
		if !strings.Contains(res.Body, want) {
			t.Fatalf("the answer lost %q: %s", want, res.Body)
		}
	}
}

func TestDoRedactsTokenInNestedMessage(t *testing.T) {
	f := &fakePlatform{respond: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"error":{"message":"login failed: %s","detail":[{"note":"retry with %s"}]},"path":"/tasks/7"}`,
			testJWT, testJWT)
	}}
	c, _ := newClient(t, f, 0)
	res, err := c.Do(context.Background(), "", Request{Method: "GET", Path: "/tasks/7"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Body, testJWT) {
		t.Fatalf("the token reached the agent: %s", res.Body)
	}
	if got := strings.Count(res.Body, redacted); got != 2 {
		t.Fatalf("expected two marks in %s, got %d", res.Body, got)
	}
	if !strings.Contains(res.Body, `"path":"/tasks/7"`) {
		t.Fatalf("the answer lost the path: %s", res.Body)
	}
}

// The counter-check: a sentence with dots, a version number and a date stays untouched.
func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	sentence := "Build 1.2.3 failed at 12.10.2026; see example.com/docs (v2.0.1) for details."
	f := &fakePlatform{respond: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, sentence)
	}}
	c, _ := newClient(t, f, 0)
	res, err := c.Do(context.Background(), "", Request{Method: "GET", Path: "/datasets"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Body != sentence {
		t.Fatalf("ordinary text was changed: %q", res.Body)
	}
	if got := RedactText(sentence); got != sentence {
		t.Fatalf("RedactText changed ordinary text: %q", got)
	}
}

// A token also stands inside punctuation, behind a scheme and with base64 padding; the rest stays.
func TestRedactTextBoundaries(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Authorization: Bearer " + testJWT + "\n", "Authorization: Bearer " + redacted + "\n"},
		{`{"token":"` + testJWT + `"}`, `{"token":"` + redacted + `"}`},
		{"(" + testJWT + ")", "(" + redacted + ")"},
		{"eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0.c2ln=", redacted},
		{"already " + redacted, "already " + redacted},
		{"eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0", "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0"}, // only two parts
	}
	for _, c := range cases {
		if got := RedactText(c.in); got != c.want {
			t.Errorf("RedactText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A failed Stream call embeds the platform's answer in its error; the token must not survive there.
func TestStreamErrorRedactsToken(t *testing.T) {
	f := &fakePlatform{respond: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "notebook export failed: %s", testJWT)
	}}
	c, _ := newClient(t, f, 0)
	body, _, err := c.Stream(context.Background(), "", Request{Method: "GET", Path: "/datasets/1/export"})
	if body != nil {
		body.Close()
	}
	if err == nil {
		t.Fatal("expected an error for a 500")
	}
	if strings.Contains(err.Error(), testJWT) {
		t.Fatalf("the token reached the agent in the error: %v", err)
	}
	if !strings.Contains(err.Error(), redacted) {
		t.Fatalf("the mark is missing: %v", err)
	}
}
