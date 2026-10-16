// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// CVAT 2.4.2 answers the "labels" field of GET /api/tasks/{id} as a page of the label list
// ({"url": ..., "count": n}), not as the labels themselves. Reading that object into a list fails
// and made the whole task unreadable, which broke annotation-summary on the instance on
// 16.10.2026. The task must be readable in both forms, and the classes then come from /api/labels.
func TestCVATTaskLabelsAsPage(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		w.Header().Set("Content-Type", "application/vnd.cvat+json")
		switch r.URL.Path {
		case "/api/tasks/7":
			// The answer of CVAT 2.4.2, shortened to the fields the service reads.
			io.WriteString(w, `{"id":7,"name":"mnist-check","size":84,
                "owner":{"username":"agent-test"},
                "labels":{"url":"https://cvat.example/api/labels?task_id=7","count":2}}`)
		case "/api/labels":
			io.WriteString(w, `{"count":2,"results":[{"id":56,"name":"0"},{"id":57,"name":"1"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := NewCVAT(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	ses := session{key: "k"}
	task, err := c.Task(context.Background(), ses, 7)
	if err != nil {
		t.Fatalf("CVAT 2.4.2's task is not readable: %v", err)
	}
	if task.ID != 7 || task.Size != 84 || task.Owner.Username != "agent-test" {
		t.Errorf("task: %+v", task)
	}
	if len(task.Labels) != 0 {
		t.Errorf("a page of the label list was read as classes: %+v", task.Labels)
	}
	// The classes come from the label list, as Labels() falls back to it when the task carries none.
	labels, err := c.Labels(context.Background(), ses, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 2 || labels[0].Name != "0" || labels[1].ID != 57 {
		t.Errorf("labels: %+v", labels)
	}
	if len(asked) != 3 || asked[1] != "/api/tasks/7" || asked[2] != "/api/labels" {
		t.Errorf("calls: %v", asked)
	}
}

// An answer that carries the labels themselves (the older form) is read as before.
func TestCVATTaskLabelsAsList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.cvat+json")
		io.WriteString(w, `{"id":7,"name":"task","size":3,"owner":{"username":"test"},
            "labels":[{"id":56,"name":"0"},{"id":57,"name":"1"}]}`)
	}))
	defer srv.Close()

	c, err := NewCVAT(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	task, err := c.Task(context.Background(), session{key: "k"}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Labels) != 2 || task.Labels[0].Name != "0" || task.Labels[1].Name != "1" {
		t.Errorf("labels from the task itself: %+v", task.Labels)
	}
}
