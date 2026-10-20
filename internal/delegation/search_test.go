// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"agw/internal/platform"
)

func TestClassifyTrainingClone(t *testing.T) {
	a := Classify(platform.Request{Method: http.MethodPost, Path: "/train/containers/07/clone", Body: []byte(`{"train_config":{"lr":0.01}}`)})
	if a.Action != Create || a.Resource != Training || a.ID != "" || a.Problem != "" || a.Effect != EffectWrite {
		t.Fatalf("clone: %+v", a)
	}
	if len(a.Also) != 1 || a.Also[0].Action != Read || a.Also[0].Resource != Training || a.Also[0].ID != "7" {
		t.Errorf("the base container is read: %+v", a.Also)
	}
	// Only train_config may be in the body: the image, the dataset and the owner come from the base.
	for _, field := range []string{"image_id", "dataset_id", "owner", "container_id", "model_filepath"} {
		b := Classify(platform.Request{Method: http.MethodPost, Path: "/train/containers/7/clone", Body: []byte(`{"train_config":{},"` + field + `":1}`)})
		if b.Problem == "" {
			t.Errorf("field %s was not refused", field)
		}
	}
	m := Classify(platform.Request{Method: http.MethodGet, Path: "/train/containers/7/metrics"})
	if m.Action != Read || m.Resource != Training || m.ID != "7" || m.Effect != EffectRead {
		t.Errorf("metrics: %+v", m)
	}
}

// The copy of a container the delegation names is allowed, of another one it is not, and a delegation without the
// right to create a training container cannot copy at all.
func TestTrainingCloneRights(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		rules string
		path  string
		want  bool
	}{
		{"own base", searchCloneRules, "/train/containers/7/clone", true},
		{"foreign base", searchCloneRules, "/train/containers/8/clone", false},
		{"no right to create", searchCloneNoCreateRules, "/train/containers/7/clone", false},
		{"metrics of the base", searchCloneRules, "/train/containers/7/metrics", true},
	}
	for _, c := range cases {
		method := http.MethodPost
		if strings.HasSuffix(c.path, "/metrics") {
			method = http.MethodGet
		}
		got, why := Evaluate(Case{Name: c.name, Rules: c.rules, Req: platform.Request{Method: method, Path: c.path, Body: bodyFor(method)}}, now)
		if got != c.want {
			t.Errorf("%s: allowed=%v, want %v (%s)", c.name, got, c.want, why)
		}
	}
}

func bodyFor(method string) []byte {
	if method == http.MethodPost {
		return []byte(`{"train_config":{"lr":0.01}}`)
	}
	return nil
}
