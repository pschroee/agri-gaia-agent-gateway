// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"testing"

	"agw/internal/platform"
)

func TestReadOf(t *testing.T) {
	cases := []struct {
		method, path, kind, id string
		ok                     bool
	}{
		{"GET", "/tasks/128", "platform_task", "128", true},
		{"GET", "/train/containers/84/status", "training_run", "84", true},
		{"GET", "/train/containers/84/logs", "training_run", "84", true},
		{"GET", "/train/containers/84/metrics", "training_run", "84", true},
		{"GET", "/tasks", "", "", false},
		{"GET", "/train/containers/84", "", "", false},
		{"POST", "/train/containers/84/run", "", "", false},
		{"DELETE", "/tasks/128", "", "", false},
	}
	for _, c := range cases {
		kind, id, ok := ReadOf(platform.Request{Method: c.method, Path: c.path})
		if ok != c.ok || kind != c.kind || id != c.id {
			t.Errorf("%s %s: got %q %q %v", c.method, c.path, kind, id, ok)
		}
	}
}

func TestTaskIDOf(t *testing.T) {
	for ref, want := range map[string]string{"task:12": "12", "task:12|train:7": "12", "12": "12", "7": "7"} {
		if got := TaskIDOf(ref); got != want {
			t.Errorf("TaskIDOf(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestReadMatches(t *testing.T) {
	cases := []struct {
		jobKind, jobRef, readKind, id string
		want                          bool
	}{
		{"platform_task", "task:128", "platform_task", "128", true},
		{"platform_task", "task:128|config:5", "platform_task", "128", true},
		{"image_build", "task:130|image:test/x:v1", "platform_task", "130", true},
		{"platform_task", "task:128", "platform_task", "129", false},
		{"training_run", "84", "training_run", "84", true},
		{"training_run", "84", "training_run", "85", false},
		{"platform_task", "task:12|train:84", "training_run", "84", true},
		{"training_run", "84", "platform_task", "84", false},
		{"notebook_run", "task:128", "platform_task", "128", false},
	}
	for _, c := range cases {
		if got := ReadMatches(c.jobKind, c.jobRef, c.readKind, c.id); got != c.want {
			t.Errorf("%s %s read %s %s: got %v", c.jobKind, c.jobRef, c.readKind, c.id, got)
		}
	}
}

func TestAnnouncedOutsidePlan(t *testing.T) {
	for _, k := range []string{"platform_task", "training_run", "image_build"} {
		if !AnnouncedOutsidePlan(k) {
			t.Errorf("%s not announced", k)
		}
	}
	for _, k := range []string{"notebook_run", "edge_deployment", ""} {
		if AnnouncedOutsidePlan(k) {
			t.Errorf("%s announced", k)
		}
	}
}
