// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"agw/internal/platform"
)

// requiredFields: PATCH /models/{id} sets every field of the body and clears the missing ones (issue #65), so
// a body must carry all of platform.ModelPatchFields. The same type as fieldRules, which init appends to: the
// allowlist of the route is checked there, the completeness here.
var requiredFields = []struct {
	method string
	re     *regexp.Regexp
	fields []string
}{
	{http.MethodPatch, regexp.MustCompile(`^/models/[^/]+$`), platform.ModelPatchFields},
}

func init() { fieldRules = append(fieldRules, requiredFields...) }

// checkRequired checks that the body of a route with required fields is a JSON object with all of them.
func checkRequired(req platform.Request) string {
	for _, rq := range requiredFields {
		if rq.method != req.Method || !rq.re.MatchString(req.Path) {
			continue
		}
		var m map[string]json.RawMessage
		if len(req.Body) == 0 || json.Unmarshal(req.Body, &m) != nil || m == nil {
			return fmt.Sprintf("%s %s needs a JSON object with all of: %s", req.Method, req.Path, strings.Join(rq.fields, ", "))
		}
		var missing []string
		for _, f := range rq.fields {
			if _, ok := m[f]; !ok {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			return "fields missing (the platform would set them to null): " + strings.Join(missing, ", ")
		}
	}
	return ""
}
