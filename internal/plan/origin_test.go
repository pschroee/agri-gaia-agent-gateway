// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"strings"
	"testing"

	"agw/internal/delegation"
	"agw/internal/store"
)

func TestDeploymentImage(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		ref     int
		literal string
	}{
		{"placeholder", `{"container_image_id":"{{step2.id}}"}`, 2, ""},
		{"number", `{"container_image_id":87}`, 0, "87"},
		{"digit string", `{"container_image_id":"87"}`, 0, "87"},
		{"zero", `{"container_image_id":0}`, 0, ""},
		{"zero string", `{"container_image_id":"0"}`, 0, ""},
		{"absent", `{"name":"x"}`, 0, ""},
		{"empty body", ``, 0, ""},
		{"not json", `nope`, 0, ""},
		{"text around placeholder", `{"container_image_id":"a{{step2.id}}"}`, 0, ""},
		{"null", `{"container_image_id":null}`, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, lit := DeploymentImage(store.PlanStep{Body: []byte(c.body)})
			if ref != c.ref || lit != c.literal {
				t.Fatalf("got (%d, %q), want (%d, %q)", ref, lit, c.ref, c.literal)
			}
		})
	}
}

func TestStepKindHelpers(t *testing.T) {
	build := store.PlanStep{Method: "POST", Path: "/container-images/build"}
	create := store.PlanStep{Method: "POST", Path: "/container-deployments"}
	deploy := store.PlanStep{Method: "PUT", Path: "/container-deployments/{{step2.id}}/deploy"}
	if !IsBuildStep(build) || IsBuildStep(create) || IsBuildStep(deploy) {
		t.Fatal("IsBuildStep")
	}
	if !IsCreateDeploymentStep(create) || IsCreateDeploymentStep(build) || IsCreateDeploymentStep(deploy) {
		t.Fatal("IsCreateDeploymentStep")
	}
	if !IsDeployStep(deploy) || IsDeployStep(create) || IsDeployStep(store.PlanStep{Method: "PUT", Path: "/container-deployments/5/undeploy"}) {
		t.Fatal("IsDeployStep")
	}
}

const (
	testOfficialURL = "https://github.com/hsos-ai-lab/agri-gaia-inference-container-template"
	testDeployPath  = "/container-deployments/{{step%d.id}}/deploy"
)

func importStep(idx int, gitURL string) store.PlanStep {
	return store.PlanStep{Idx: idx, Method: "POST", Path: "/inference-container-templates/create/git", Effect: "write",
		Form: []byte(`{"git_url":["` + gitURL + `"],"name":["t"]}`)}
}

func buildStepWith(idx int, template string) store.PlanStep {
	return store.PlanStep{Idx: idx, Method: "POST", Path: "/container-images/build", Effect: "compute",
		Body: []byte(`{"container_template_id":` + template + `,"model_id":6,"repository":"me/img","tag":"v1","architecture":"amd64"}`)}
}

func createStepWith(idx int, image string) store.PlanStep {
	return store.PlanStep{Idx: idx, Method: "POST", Path: "/container-deployments", Effect: "write",
		Body: []byte(`{"edge_device_id":1,"container_image_id":` + image + `,"name":"agw-x"}`)}
}

func startStep(idx, create int) store.PlanStep {
	return store.PlanStep{Idx: idx, Method: "PUT", Path: strings.Replace(testDeployPath, "%d", string(rune('0'+create)), 1), Effect: "compute", ConfirmEach: true}
}

func okTemplates(ids ...string) TemplateOK {
	return func(id string) bool {
		for _, i := range ids {
			if i == id {
				return true
			}
		}
		return false
	}
}

func chainSteps(template, image string) []store.PlanStep {
	return []store.PlanStep{buildStepWith(1, template), createStepWith(2, image), startStep(3, 2)}
}

func TestImageOriginsSamePlanTool(t *testing.T) {
	steps := chainSteps("4", `"{{step1.id}}"`)
	steps[0].Tool, steps[1].Tool = "build_inference_image", "create_deployment"
	ImageOrigins(steps, nil, okTemplates("4"))
	o := steps[2].ImageOrigin
	if o == nil || o.Step != 1 || o.PlanID != "" || !steps[2].Covered || steps[2].UncoveredReason != "" {
		t.Fatalf("start: %+v", steps[2])
	}
	if !steps[0].Covered || !steps[1].Covered {
		t.Fatal("build and create stay covered")
	}
}

func TestImageOriginsRawSteps(t *testing.T) {
	steps := chainSteps("4", `"{{step1.id}}"`)
	ImageOrigins(steps, nil, okTemplates("4"))
	if steps[2].ImageOrigin == nil || !steps[2].Covered {
		t.Fatalf("raw chain: %+v", steps[2])
	}
}

func TestImageOriginsLiteralImage(t *testing.T) {
	steps := chainSteps("4", `87`)
	ImageOrigins(steps, nil, okTemplates("4"))
	if steps[2].Covered || steps[2].ImageOrigin != nil || steps[2].UncoveredReason != "image_origin" {
		t.Fatalf("start: %+v", steps[2])
	}
}

func replacedPlan(state, createdID string) *store.Plan {
	b := buildStepWith(8, "4")
	b.State, b.CreatedResource, b.CreatedID = state, delegation.ContainerImage, createdID
	return &store.Plan{ID: "7c1f231f-aaaa-bbbb-cccc-000000000000", Steps: []store.PlanStep{b}}
}

func demoSteps() []store.PlanStep {
	return []store.PlanStep{createStepWith(1, `"87"`), startStep(2, 1)}
}

func TestImageOriginsReplacedPlan(t *testing.T) {
	steps := demoSteps()
	ImageOrigins(steps, replacedPlan(StepDone, "87"), okTemplates("4"))
	o := steps[1].ImageOrigin
	if o == nil || o.PlanID != "7c1f231f-aaaa-bbbb-cccc-000000000000" || o.Step != 8 || !steps[1].Covered {
		t.Fatalf("start: %+v", steps[1])
	}
}

func TestImageOriginsReplacedBuildNotDone(t *testing.T) {
	steps := demoSteps()
	ImageOrigins(steps, replacedPlan(StepOpen, "87"), okTemplates("4"))
	if steps[1].Covered || steps[1].ImageOrigin != nil {
		t.Fatalf("start: %+v", steps[1])
	}
}

func TestImageOriginsReplacedOtherImage(t *testing.T) {
	steps := demoSteps()
	ImageOrigins(steps, replacedPlan(StepDone, "88"), okTemplates("4"))
	if steps[1].Covered || steps[1].ImageOrigin != nil {
		t.Fatalf("start: %+v", steps[1])
	}
	ImageOrigins(steps, nil, okTemplates("4"))
	if steps[1].Covered {
		t.Fatal("no replaced plan, no origin for a literal id")
	}
}

func TestImageOriginsForeignTemplate(t *testing.T) {
	steps := chainSteps("9", `"{{step1.id}}"`)
	ImageOrigins(steps, nil, okTemplates("4"))
	if steps[2].Covered || steps[2].UncoveredReason != "image_origin" {
		t.Fatalf("start: %+v", steps[2])
	}
	steps = demoSteps()
	r := replacedPlan(StepDone, "87")
	r.Steps[0] = func() store.PlanStep {
		s := buildStepWith(8, "9")
		s.State, s.CreatedResource, s.CreatedID = StepDone, delegation.ContainerImage, "87"
		return s
	}()
	ImageOrigins(steps, r, okTemplates("4"))
	if steps[1].Covered {
		t.Fatal("replaced build with foreign template must not cover")
	}
}

func TestImageOriginsTemplateNil(t *testing.T) {
	steps := chainSteps("4", `"{{step1.id}}"`)
	ImageOrigins(steps, nil, nil)
	if steps[2].Covered {
		t.Fatalf("start: %+v", steps[2])
	}
}

func TestImageOriginsImportTemplateStep(t *testing.T) {
	good := []store.PlanStep{importStep(1, testOfficialURL), buildStepWith(2, `"{{step1.id}}"`), createStepWith(3, `"{{step2.id}}"`), startStep(4, 3)}
	ImageOrigins(good, nil, nil)
	if !good[3].Covered || good[3].ImageOrigin == nil || good[3].ImageOrigin.Step != 2 {
		t.Fatalf("official import: %+v", good[3])
	}
	bad := []store.PlanStep{importStep(1, "https://example.invalid/t"), buildStepWith(2, `"{{step1.id}}"`), createStepWith(3, `"{{step2.id}}"`), startStep(4, 3)}
	ImageOrigins(bad, nil, nil)
	if bad[3].Covered {
		t.Fatalf("foreign import: %+v", bad[3])
	}
	notImport := []store.PlanStep{createStepWith(1, `5`), buildStepWith(2, `"{{step1.id}}"`), createStepWith(3, `"{{step2.id}}"`), startStep(4, 3)}
	ImageOrigins(notImport, nil, nil)
	if notImport[3].Covered {
		t.Fatalf("placeholder on a non-import step: %+v", notImport[3])
	}
}

// The import of the OpenVINO template of the fork covers the start like the Triton import (#230).
func TestImageOriginsOVMSImportStep(t *testing.T) {
	const forkURL = "https://github.com/pschroee/agri-gaia-inference-container-template"
	steps := []store.PlanStep{importStep(1, forkURL), buildStepWith(2, `"{{step1.id}}"`), createStepWith(3, `"{{step2.id}}"`), startStep(4, 3)}
	ImageOrigins(steps, nil, nil)
	if !steps[3].Covered || steps[3].ImageOrigin == nil || steps[3].ImageOrigin.Step != 2 {
		t.Fatalf("fork import: %+v", steps[3])
	}
	other := []store.PlanStep{importStep(1, forkURL+".git"), buildStepWith(2, `"{{step1.id}}"`), createStepWith(3, `"{{step2.id}}"`), startStep(4, 3)}
	ImageOrigins(other, nil, nil)
	if other[3].Covered {
		t.Fatalf("fork URL with .git: %+v", other[3])
	}
}

func TestImageOriginsLiteralDeploymentPath(t *testing.T) {
	steps := chainSteps("4", `"{{step1.id}}"`)
	steps[2].Path = "/container-deployments/55/deploy"
	ImageOrigins(steps, nil, okTemplates("4"))
	if steps[2].Covered || steps[2].UncoveredReason != "image_origin" {
		t.Fatalf("start: %+v", steps[2])
	}
}

func TestImageOriginsCreateRefersToNonBuild(t *testing.T) {
	steps := []store.PlanStep{
		{Idx: 1, Method: "POST", Path: "/models", Effect: "write"},
		createStepWith(2, `"{{step1.id}}"`), startStep(3, 2),
	}
	ImageOrigins(steps, nil, okTemplates("4"))
	if steps[2].Covered {
		t.Fatalf("start: %+v", steps[2])
	}
	// the start points at a step that does not create a deployment
	steps = chainSteps("4", `"{{step1.id}}"`)
	steps[2] = startStep(3, 1)
	ImageOrigins(steps, nil, okTemplates("4"))
	if steps[2].Covered {
		t.Fatalf("start of a build step: %+v", steps[2])
	}
}

func TestImageOriginsNotebookStaysUncovered(t *testing.T) {
	steps := []store.PlanStep{buildStepWith(1, "4"),
		{Idx: 2, Method: "POST", Path: "/_agw/notebook/runs", Effect: "compute", ConfirmEach: true,
			ImageOrigin: &store.ImageOrigin{Step: 1}}}
	ImageOrigins(steps, nil, okTemplates("4"))
	if steps[1].Covered || steps[1].ImageOrigin != nil || steps[1].UncoveredReason != "runs_code" {
		t.Fatalf("notebook: %+v", steps[1])
	}
}

// replacedRestartPlan is a replaced plan of the demo run (#231): a done build (step 8, template 4), a done
// create step that created deployment 9 from image {{step8.id}} and a failed start.
func replacedRestartPlan(createdDeployment string, buildState string) *store.Plan {
	b := buildStepWith(8, "4")
	b.State, b.CreatedResource, b.CreatedID = buildState, delegation.ContainerImage, "87"
	c := createStepWith(9, `"{{step8.id}}"`)
	c.State, c.CreatedResource, c.CreatedID = StepDone, delegation.ContainerDeployment, createdDeployment
	return &store.Plan{ID: "7c1f231f-aaaa-bbbb-cccc-000000000000", Steps: []store.PlanStep{b, c}}
}

func restartStep(idx int, deployment string) store.PlanStep {
	return store.PlanStep{Idx: idx, Method: "PUT", Path: "/container-deployments/" + deployment + "/deploy", Effect: "compute", ConfirmEach: true}
}

func TestImageOriginsRestartOfReplacedDeployment(t *testing.T) {
	steps := []store.PlanStep{restartStep(1, "55")}
	ImageOrigins(steps, replacedRestartPlan("55", StepDone), okTemplates("4"))
	o := steps[0].ImageOrigin
	if o == nil || o.PlanID != "7c1f231f-aaaa-bbbb-cccc-000000000000" || o.Step != 8 || !steps[0].Covered {
		t.Fatalf("start: %+v", steps[0])
	}
}

func TestImageOriginsRestartNotCoveredWithoutDoneBuild(t *testing.T) {
	steps := []store.PlanStep{restartStep(1, "55")}
	ImageOrigins(steps, replacedRestartPlan("55", StepOpen), okTemplates("4"))
	if steps[0].Covered || steps[0].ImageOrigin != nil {
		t.Fatalf("start: %+v", steps[0])
	}
	// A foreign template is not covered either.
	steps = []store.PlanStep{restartStep(1, "55")}
	ImageOrigins(steps, replacedRestartPlan("55", StepDone), okTemplates("9"))
	if steps[0].Covered || steps[0].ImageOrigin != nil {
		t.Fatalf("foreign template: %+v", steps[0])
	}
	// Without a replaced plan a literal deployment id stays uncovered.
	steps = []store.PlanStep{restartStep(1, "55")}
	ImageOrigins(steps, nil, okTemplates("4"))
	if steps[0].Covered || steps[0].ImageOrigin != nil {
		t.Fatalf("no replaced plan: %+v", steps[0])
	}
}

func TestImageOriginsRestartOtherDeployment(t *testing.T) {
	steps := []store.PlanStep{restartStep(1, "56")}
	ImageOrigins(steps, replacedRestartPlan("55", StepDone), okTemplates("4"))
	if steps[0].Covered || steps[0].ImageOrigin != nil {
		t.Fatalf("start of a deployment the replaced plan did not create: %+v", steps[0])
	}
}
