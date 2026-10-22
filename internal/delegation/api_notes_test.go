// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"strings"
	"testing"

	"agw/internal/platform"
)

// The REST index must not offer a field the gateway refuses (#210): the body of a note stays within fieldRules.
func TestAPIPathNotesRespectFieldRules(t *testing.T) {
	cases := []struct{ method, key string }{
		{"PATCH", "/datasets/{dataset_id}"},
		{"PATCH", "/train/containers/{train_container_id}"},
	}
	for _, c := range cases {
		body, ok := platform.APIPathBody(c.method, c.key)
		if !ok || body == "" {
			t.Errorf("%s %s: no note with a body", c.method, c.key)
			continue
		}
		path := strings.NewReplacer("{dataset_id}", "5", "{train_container_id}", "7").Replace(c.key)
		matched := false
		for _, fr := range fieldRules {
			if fr.method != c.method || !fr.re.MatchString(path) {
				continue
			}
			matched = true
			if len(fr.fields) == 0 {
				if !strings.HasPrefix(body, "none") {
					t.Errorf("%s %s: rule allows no field, note says %q", c.method, c.key, body)
				}
				continue
			}
			for _, f := range strings.Split(body, ",") {
				f = strings.TrimSuffix(strings.TrimSpace(f), "*")
				if !contains(fr.fields, f) {
					t.Errorf("%s %s: note names %q, the rule allows %v", c.method, c.key, f, fr.fields)
				}
			}
		}
		if !matched {
			t.Errorf("%s %s: no field rule", c.method, c.key)
		}
	}
}
