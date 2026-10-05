package delegation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agw/internal/platform"
)

func TestConformanceWithoutPlatform(t *testing.T) {
	cases := ConformanceCases()
	results := map[string]bool{}
	forbidden, allowedOK := 0, 0
	for _, c := range cases {
		got, why := Evaluate(c, time.Now())
		results[c.Name] = got
		if got != c.Allowed {
			t.Errorf("%s [%s]: erlaubt=%v, Soll %v (%s)", c.Name, c.Group, got, c.Allowed, why)
		}
		if !c.Allowed {
			forbidden++
		} else if got {
			allowedOK++
		}
	}
	t.Logf("%d verbotene und %d erlaubte Fälle\n%s", forbidden, len(cases)-forbidden, Report(results, cases))
	if forbidden < 30 {
		t.Fatalf("zu wenige verbotene Fälle: %d", forbidden)
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		`{"rules":[{"action":"write","resource":"dataset"}]}`,
		`{"rules":[{"action":"read","resource":"datasets"}]}`,
		`{"rules":[{"action":"read","resource":"dataset","ids":[""]}]}`,
		`{"rules":[],"confirm":"immer"}`,
		`{"rules":[],"expires":"2026-01-01T00:00:00Z"}`, // Tippfehler im Feldnamen
		`{"rules":[]} {"rules":[]}`,
		`{"rules":[{"action":"update","resource":"training","ids":["?"]}]}`, // Review 5, M3
	}
	for _, b := range bad {
		if _, err := Parse([]byte(b)); err == nil {
			t.Errorf("erwartet abgewiesen: %s", b)
		}
	}
	d, err := Parse([]byte(`{"rules":[{"action":"run","resource":"training","ids":[" 07 ","own","*"]}]}`))
	if err != nil || strings.Join(d.Rules[0].IDs, ",") != "7,own,*" || !d.Enforcing() || !d.ConfirmWrites() {
		t.Fatalf("Parse: %+v %v", d, err)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		m, p string
		want string
	}{
		{"GET", "/datasets", "read dataset"},
		{"GET", "/datasets/keyword", "read dataset"},
		{"GET", "/datasets/007", "read dataset 7"},
		{"GET", "/train/config/Torchvision/Mask R-CNN", "read train_template"},
		{"GET", "/train/containers/3/model", "create model + read training 3"},
		{"GET", "/datasets/3/download", "update dataset 3"},
		{"DELETE", "/edge-devices/3", "delete edge_device 3"},
		{"DELETE", "/integrated-services/3", "delete api /integrated-services/3"},
		{"GET", "/container-images/test/mnist:v1", "read container_image test/mnist:v1"},
		{"GET", "/agrovoc/keywords", "read api /agrovoc/keywords"},
		{"POST", "/agrovoc/keywords", "update api /agrovoc/keywords"},
		{"GET", "/openapi.json", "read docs"},
	}
	for _, c := range cases {
		if got := Classify(platform.Request{Method: c.m, Path: c.p}).String(); got != c.want {
			t.Errorf("%s %s: %q, erwartet %q", c.m, c.p, got, c.want)
		}
	}
	a := Classify(platform.Request{Method: "PUT", Path: "/train/config", Body: json.RawMessage(`{"container_id":7,"container_id":9,"dataset_id":12}`)})
	if a.String() != "update training 9 + read dataset 12" {
		t.Fatalf("PUT /train/config: %s", a)
	}
}

func TestCreated(t *testing.T) {
	res, id, ok := Created(Access{Action: Create, Resource: Dataset}, platform.Result{Status: "ok", HTTPStatus: 201, Body: `{"id":12,"name":"x"}`})
	if !ok || res != Dataset || id != "12" {
		t.Fatalf("Datensatz: %s %s %v", res, id, ok)
	}
	res, id, ok = Created(Access{Action: Create, Resource: Training}, platform.Result{Status: "ok", HTTPStatus: 202, Location: "/tasks/40"})
	if !ok || res != Task || id != "40" {
		t.Fatalf("Training: %s %s %v", res, id, ok)
	}
	if _, _, ok := Created(Access{Action: Create, Resource: Dataset}, platform.Result{Status: "error", HTTPStatus: 500, Body: `{"id":1}`}); ok {
		t.Fatal("Fehlschlag darf keine Herkunft erzeugen")
	}
	if _, _, ok := Created(Access{Action: Update, Resource: Dataset}, platform.Result{Status: "ok", HTTPStatus: 200, Body: `{"id":5}`}); ok {
		t.Fatal("nur Anlage-Aufrufe erzeugen Herkunft")
	}
}

func TestSummary(t *testing.T) {
	d, _ := Parse([]byte(ConformanceDelegation))
	s := d.Summary()
	if !strings.Contains(s, "update dataset: in diesem Chat angelegte") || !strings.Contains(s, "read dataset: alle") {
		t.Fatalf("Zusammenfassung: %s", s)
	}
}
