// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"net/http"
	"regexp"

	"agw/internal/platform"
)

// ContainerTemplate is an inference container template (issue #67).
const ContainerTemplate = "container_template"

// The routes of inference images and their templates (issue #67). init appends them, so that other
// routes can be added in parallel.
func init() {
	resources[ContainerTemplate] = true
	build := r(http.MethodPost, `/container-images/build`, Create, ContainerImage, 0)
	build.effect = EffectCompute // a build runs for minutes on the platform
	routes = append(routes,
		build,
		r(http.MethodGet, `/inference-container-templates`, Read, ContainerTemplate, 0),
		r(http.MethodPost, `/inference-container-templates/create/git`, Create, ContainerTemplate, 0),
	)
	fieldRules = append(fieldRules, struct {
		method string
		re     *regexp.Regexp
		fields []string
	}{http.MethodPost, regexp.MustCompile(`^/container-images/build$`),
		[]string{"container_template_id", "repository", "tag", "model_id", "architecture"}})
}

// alsoInBody names the objects that a call takes from its body besides its own route (issue #67):
// building an image reads the model and the template named there. The key is method and route pattern.
var alsoInBody = map[string][]struct{ key, resource string }{
	"POST ^/container-images/build$": {{"model_id", Model}, {"container_template_id", ContainerTemplate}},
}

// extraAlso returns the accesses of the body identifiers of a route (see alsoInBody).
func extraAlso(rt route, req platform.Request) []Access {
	var out []Access
	for _, x := range alsoInBody[rt.method+" "+rt.re.String()] {
		out = append(out, Access{Action: Read, Resource: x.resource, ID: bodyIDOr(req.Body, x.key)})
	}
	return out
}
