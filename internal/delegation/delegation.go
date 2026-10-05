// Package delegation prüft Plattform-Aufrufe gegen die übertragenen Rechte eines Chats: welche
// Aktionen auf welchen Objekten der Agent für diese Aufgabe ausführen darf. Alles andere ist ein
// Übergriff. Jeder Aufruf wird allein aus Methode und normalisiertem Pfad eingeordnet, nie aus
// Angaben des Agenten (docs/plan-delegation-rest-plattform.md, Schritt 1).
package delegation

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"agw/internal/platform"
)

// Aktionen.
const (
	Read   = "read"
	Create = "create"
	Update = "update"
	Delete = "delete"
	Run    = "run"
)

// Ressourcen.
const (
	Dataset        = "dataset"
	Model          = "model"
	Training       = "training"
	Task           = "task"
	TrainTemplate  = "train_template"
	EdgeDevice     = "edge_device"
	ContainerImage = "container_image"
	API            = "api"  // alle übrigen Pfade
	Docs           = "docs" // Beschreibung der API, immer lesbar
)

// Besondere Kennungen in Regeln.
const (
	All = "*"   // jedes Objekt
	Own = "own" // in dieser Delegation entstanden (Herkunftsregel)
)

var (
	actions   = map[string]bool{Read: true, Create: true, Update: true, Delete: true, Run: true}
	resources = map[string]bool{Dataset: true, Model: true, Training: true, Task: true, TrainTemplate: true, EdgeDevice: true, ContainerImage: true, API: true}
)

// Rule erlaubt eine Aktion auf einer Ressource. Ohne IDs gilt sie nur für Aufrufe ohne Objekt
// (Listen, Anlegen); IDs nennt Kennungen, "*" oder "own".
type Rule struct {
	Action   string   `json:"action"`
	Resource string   `json:"resource"`
	IDs      []string `json:"ids,omitempty"`
}

// Delegation sind die übertragenen Rechte eines Chats.
type Delegation struct {
	Rules     []Rule     `json:"rules"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Enforce: true (Standard) weist Übergriffe ab; false protokolliert sie nur (Stufe „keine
	// Schutzmaßnahme" des Versuchsplans: dieselbe Beobachtungsstelle, keine Grenze).
	Enforce *bool `json:"enforce,omitempty"`
	// Confirm: "writes" (Standard) fragt bei schreibenden Aufrufen zusätzlich den Nutzer, "none" nicht.
	Confirm string `json:"confirm,omitempty"`
}

// Parse liest und prüft eine Delegation. Unbekannte Felder sind ein Fehler, damit ein Tippfehler
// nicht still Rechte verschenkt oder entzieht.
func Parse(raw []byte) (*Delegation, error) {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	var del Delegation
	if err := d.Decode(&del); err != nil {
		return nil, fmt.Errorf("Delegation unlesbar: %w", err)
	}
	if d.More() {
		return nil, errors.New("Delegation: nach dem Objekt folgt weiterer Text")
	}
	return &del, del.Validate()
}

// Validate prüft Aktionen, Ressourcen und Kennungen und bringt Kennungen in eine feste Form.
func (d *Delegation) Validate() error {
	for i := range d.Rules {
		r := &d.Rules[i]
		if !actions[r.Action] {
			return fmt.Errorf("Regel %d: unbekannte Aktion %q (read, create, update, delete, run)", i+1, r.Action)
		}
		if !resources[r.Resource] {
			return fmt.Errorf("Regel %d: unbekannte Ressource %q", i+1, r.Resource)
		}
		for j, id := range r.IDs {
			id = strings.TrimSpace(id)
			if id == "" {
				return fmt.Errorf("Regel %d: leere Kennung", i+1)
			}
			if id == unknownID {
				return fmt.Errorf("Regel %d: %q ist keine Kennung", i+1, id)
			}
			if id != All && id != Own {
				id = canonID(id)
			}
			r.IDs[j] = id
		}
	}
	switch d.Confirm {
	case "", "writes", "none":
	default:
		return fmt.Errorf("confirm muss writes oder none sein, nicht %q", d.Confirm)
	}
	return nil
}

// Enforcing sagt, ob Übergriffe abgewiesen werden.
func (d *Delegation) Enforcing() bool { return d.Enforce == nil || *d.Enforce }

// ConfirmWrites sagt, ob schreibende Aufrufe zusätzlich der Nutzer bestätigt.
func (d *Delegation) ConfirmWrites() bool { return d.Confirm != "none" }

// Summary beschreibt die Rechte für den Agenten und die Oberfläche.
func (d *Delegation) Summary() string {
	var b strings.Builder
	mode := "Übergriffe werden abgewiesen"
	if !d.Enforcing() {
		mode = "Übergriffe werden nur protokolliert"
	}
	fmt.Fprintf(&b, "Übertragene Rechte (%s", mode)
	if d.ExpiresAt != nil {
		fmt.Fprintf(&b, ", gültig bis %s", d.ExpiresAt.Format(time.RFC3339))
	}
	b.WriteString("):\n")
	if len(d.Rules) == 0 {
		b.WriteString("  keine; jeder Aufruf der Plattform ist ein Übergriff\n")
	}
	for _, r := range d.Rules {
		ids := "ohne Objekt (Listen, Anlegen)"
		if len(r.IDs) > 0 {
			names := make([]string, len(r.IDs))
			for i, id := range r.IDs {
				switch id {
				case All:
					names[i] = "alle"
				case Own:
					names[i] = "in diesem Chat angelegte"
				default:
					names[i] = id
				}
			}
			ids = strings.Join(names, ", ")
		}
		fmt.Fprintf(&b, "  %s %s: %s\n", r.Action, r.Resource, ids)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Access ist die Einordnung eines Aufrufs. Manche Aufrufe berühren mehr als ein Objekt (Review 5,
// K2 und W1): Also nennt die weiteren Zugriffe, die ebenfalls erlaubt sein müssen. Problem ist gesetzt,
// wenn der Aufruf unabhängig von den Regeln abzuweisen ist (etwa ein Feld außerhalb der Positivliste).
type Access struct {
	Action   string   `json:"action"`
	Resource string   `json:"resource"`
	ID       string   `json:"id,omitempty"`
	Also     []Access `json:"also,omitempty"`
	Problem  string   `json:"problem,omitempty"`
}

func (a Access) String() string {
	s := a.Action + " " + a.Resource
	if a.ID != "" {
		s += " " + a.ID
	}
	for _, x := range a.Also {
		s += " + " + x.String()
	}
	return s
}

// unknownID steht für eine Kennung, die im Aufruf fehlt oder unlesbar ist; keine Regel nennt sie.
const unknownID = "?"

type route struct {
	method   string
	re       *regexp.Regexp
	action   string
	resource string
	idGroup  int // Gruppe mit der Kennung, 0: ohne Objekt
}

func r(method, pattern, action, resource string, idGroup int) route {
	return route{method, regexp.MustCompile("^" + pattern + "$"), action, resource, idGroup}
}

const seg = `([^/]+)`

// routes ordnet die Pfade der Plattform ein, die festen vor denen mit Kennung (sonst wäre
// /datasets/keyword ein Datensatz „keyword"). Was hier fehlt, ist api.
var routes = []route{
	r("GET", `/openapi\.json`, Read, Docs, 0),
	r("GET", `/datasets`, Read, Dataset, 0),
	r("GET", `/datasets/keyword`, Read, Dataset, 0),
	r("GET", `/datasets/catalogue`, Read, Dataset, 0),
	r("GET", `/datasets/convert/formats`, Read, Dataset, 0),
	r("POST", `/datasets`, Create, Dataset, 0),
	r("POST", `/datasets/import`, Create, Dataset, 0),
	r("GET", `/datasets/`+seg, Read, Dataset, 1),
	// Schreibendes GET: holt die Annotation aus CVAT und schreibt sie nach MinIO (Review 5, W5).
	r("GET", `/datasets/`+seg+`/download`, Update, Dataset, 1),
	r("PATCH", `/datasets/`+seg, Update, Dataset, 1),
	r("PATCH", `/datasets/`+seg+`/toggle-public`, Update, Dataset, 1),
	r("POST", `/datasets/`+seg+`/annotate`, Update, Dataset, 1),
	r("DELETE", `/datasets/`+seg, Delete, Dataset, 1),

	r("GET", `/models`, Read, Model, 0),
	r("GET", `/models/keyword`, Read, Model, 0),
	r("POST", `/models`, Create, Model, 0),
	r("GET", `/models/`+seg, Read, Model, 1),
	r("GET", `/models/`+seg+`/download`, Read, Model, 1),
	r("PATCH", `/models/`+seg, Update, Model, 1),
	r("PATCH", `/models/`+seg+`/toggle-public`, Update, Model, 1),
	r("DELETE", `/models/`+seg, Delete, Model, 1),

	r("GET", `/train/providers`, Read, TrainTemplate, 0),
	r("GET", `/train/architectures/`+seg, Read, TrainTemplate, 0),
	r("GET", `/train/config/export`, Read, TrainTemplate, 0),
	r("GET", `/train/config/`+seg+`/`+seg, Read, TrainTemplate, 0),
	r("POST", `/train/config`, Create, Training, 0),
	r("PUT", `/train/config`, Update, Training, 0), // Kennung steht im Körper (container_id)
	r("GET", `/train/containers`, Read, Training, 0),
	r("GET", `/train/containers/`+seg, Read, Training, 1),
	r("GET", `/train/containers/`+seg+`/status`, Read, Training, 1),
	r("GET", `/train/containers/`+seg+`/logs`, Read, Training, 1),
	r("GET", `/train/containers/`+seg+`/config`, Read, Training, 1),
	// Schreibendes GET: legt aus dem Container ein Modell an (Review K1 der Plattform-Anbindung).
	r("GET", `/train/containers/`+seg+`/model`, Create, Model, 0),
	r("POST", `/train/containers/`+seg+`/run`, Run, Training, 1),
	r("POST", `/train/containers/`+seg+`/stop`, Run, Training, 1),
	r("PATCH", `/train/containers/`+seg, Update, Training, 1),
	r("PATCH", `/train/containers/`+seg+`/score`, Update, Training, 1),
	r("DELETE", `/train/containers/`+seg, Delete, Training, 1),

	r("GET", `/tasks`, Read, Task, 0),
	r("GET", `/tasks/`+seg, Read, Task, 1),
	r("DELETE", `/tasks/`+seg, Delete, Task, 1),

	r("GET", `/edge-devices`, Read, EdgeDevice, 0),
	r("POST", `/edge-devices`, Create, EdgeDevice, 0),
	// Schreibendes GET: registriert ein noch nicht registriertes Gerät bei Portainer (Review 5, W5).
	r("GET", `/edge-devices/`+seg, Update, EdgeDevice, 1),
	r("DELETE", `/edge-devices/`+seg, Delete, EdgeDevice, 1),
	r("GET", `/edge-devices/`+seg+`/deployments`, Read, EdgeDevice, 1),

	r("GET", `/container-images`, Read, ContainerImage, 0),
	r("GET", `/container-images/([^/]+/[^/]+:[^/]+)`, Read, ContainerImage, 1),
	r("DELETE", `/container-images/([^/]+/[^/]+:[^/]+)`, Delete, ContainerImage, 1),
}

// Classify ordnet einen (normalisierten) Aufruf ein. Unbekannte Pfade sind api: GET read, sonst
// update; erlaubt nur mit einer ausdrücklichen api-Regel.
func Classify(req platform.Request) Access {
	for _, rt := range routes {
		if rt.method != req.Method {
			continue
		}
		m := rt.re.FindStringSubmatch(req.Path)
		if m == nil {
			continue
		}
		a := Access{Action: rt.action, Resource: rt.resource}
		if rt.idGroup > 0 {
			a.ID = canonID(m[rt.idGroup])
		}
		switch {
		case rt.method == http.MethodPut && rt.re.String() == "^/train/config$":
			// Kennungen im Körper (Review 5, W1): Container und Datensatz.
			a.ID = bodyIDOr(req.Body, "container_id")
			a.Also = append(a.Also, Access{Action: Read, Resource: Dataset, ID: bodyIDOr(req.Body, "dataset_id")})
		case rt.method == http.MethodPost && rt.re.String() == "^/train/config$":
			a.Also = append(a.Also, Access{Action: Read, Resource: Dataset, ID: bodyIDOr(req.Body, "dataset_id")})
		case rt.re.String() == "^/train/containers/"+seg+"/model$":
			// Das neue Modell enthält Datei und Protokoll des Containers (Review 5, K2).
			a.Also = append(a.Also, Access{Action: Read, Resource: Training, ID: canonID(m[1])})
		}
		if p := checkFields(req); p != "" {
			a.Problem = p
		}
		return a
	}
	switch {
	case req.Method == http.MethodGet && !req.Writes():
		return Access{Action: Read, Resource: API, ID: req.Path}
	case req.Method == http.MethodDelete:
		return Access{Action: Delete, Resource: API, ID: req.Path} // Review 5, M2
	}
	return Access{Action: Update, Resource: API, ID: req.Path}
}

// fieldRules: Positivlisten für schreibende Körper (Review 5, K1). PATCH /datasets/{id} setzt im
// Backend jedes Feld per setattr, auch bucket_name (später ungeprüft in einem Shell-Befehl im
// CVAT-Container) und metadata_uri (Löschen fremder Metadaten); der PATCH der Trainingscontainer
// übernimmt container_id, image_id und owner. Wer hier ein Feld ergänzt, prüft es am Backend-Code.
var fieldRules = []struct {
	method string
	re     *regexp.Regexp
	fields []string
}{
	{http.MethodPatch, regexp.MustCompile(`^/datasets/[^/]+$`), []string{"name", "description"}},
	{http.MethodPatch, regexp.MustCompile(`^/train/containers/[^/]+$`), nil},
	{http.MethodPatch, regexp.MustCompile(`^/train/containers/[^/]+/score$`), []string{"score"}},
}

// checkFields prüft die Felder eines JSON-Körpers gegen die Positivliste der Route.
func checkFields(req platform.Request) string {
	for _, fr := range fieldRules {
		if fr.method != req.Method || !fr.re.MatchString(req.Path) {
			continue
		}
		if len(req.Body) == 0 {
			return ""
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(req.Body, &m) != nil {
			return "Körper ist kein JSON-Objekt"
		}
		for k := range m {
			if !contains(fr.fields, k) {
				if len(fr.fields) == 0 {
					return fmt.Sprintf("%s %s: kein Feld darf gesetzt werden (%q)", req.Method, fr.re.String(), k)
				}
				return fmt.Sprintf("Feld %q nicht erlaubt; erlaubt: %s", k, strings.Join(fr.fields, ", "))
			}
		}
	}
	return ""
}

// bodyIDOr ist bodyID mit unknownID, wenn die Kennung fehlt (dann passt nur eine Regel mit "*").
func bodyIDOr(body []byte, key string) string {
	if id := bodyID(body, key); id != "" {
		return id
	}
	return unknownID
}

// canonID bringt eine Kennung in eine feste Form: Ganzzahlen ohne führende Nullen und Leerzeichen
// (das Backend liest „01" als 1), alles andere unverändert.
func canonID(s string) string {
	t := strings.TrimSpace(s)
	if n, err := strconv.ParseInt(t, 10, 64); err == nil {
		return strconv.FormatInt(n, 10)
	}
	return s
}

// bodyID liest eine Kennung aus dem JSON-Körper so, wie das Backend (Python json) sie liest: bei
// doppelten Schlüsseln gilt der letzte; Go macht es beim Lesen in eine Map ebenso.
func bodyID(body []byte, key string) string {
	// Zahlen exakt lesen (Review 5, W2): float64 machte aus 9007199254740993 die Kennung …992.
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.UseNumber()
	var m map[string]any
	if d.Decode(&m) != nil {
		return ""
	}
	switch v := m[key].(type) {
	case json.Number:
		if n, err := strconv.ParseInt(v.String(), 10, 64); err == nil {
			return strconv.FormatInt(n, 10)
		}
		return "" // 7.0, 1e3 und Überlauf: keine Ganzzahl, keine Kennung
	case string:
		return canonID(v)
	}
	return ""
}

// Decision ist das Ergebnis einer Prüfung.
type Decision struct {
	Allowed bool
	Access  Access
	Reason  string // bei Übergriffen: warum
}

// Check prüft einen Zugriff. own sagt, ob ein Objekt in dieser Delegation entstanden ist.
func (d *Delegation) Check(a Access, now time.Time, own func(resource, id string) bool) Decision {
	dec := Decision{Access: a}
	if a.Problem != "" {
		dec.Reason = a.Problem
		return dec
	}
	if a.Resource == Docs {
		dec.Allowed = true
		return dec
	}
	if d.ExpiresAt != nil && !now.Before(*d.ExpiresAt) {
		dec.Reason = "Delegation abgelaufen seit " + d.ExpiresAt.Format(time.RFC3339)
		return dec
	}
	for _, x := range a.Also {
		if sub := d.checkOne(x, own); !sub {
			dec.Reason = "nicht in den übertragenen Rechten: " + x.Action + " " + x.Resource + " " + x.ID + " (berührt von " + a.Action + " " + a.Resource + ")"
			return dec
		}
	}
	if d.checkOne(a, own) {
		dec.Allowed = true
		return dec
	}
	dec.Reason = "nicht in den übertragenen Rechten: " + Access{Action: a.Action, Resource: a.Resource, ID: a.ID}.String()
	return dec
}

func (d *Delegation) checkOne(a Access, own func(resource, id string) bool) bool {
	for _, r := range d.Rules {
		if r.Action != a.Action || r.Resource != a.Resource {
			continue
		}
		if a.ID == "" {
			if len(r.IDs) == 0 || contains(r.IDs, All) {
				return true
			}
			continue
		}
		for _, id := range r.IDs {
			if id == All || id == a.ID || (id == Own && own != nil && a.ID != unknownID && own(a.Resource, a.ID)) {
				return true
			}
		}
	}
	return false
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Created ermittelt nach einem erfolgreichen Anlage-Aufruf das neue Objekt (Herkunftsregel): die
// Kennung aus dem Körper bei Datensätzen und Modellen, die Aufgabe aus Location beim Training.
// Trainingscontainer entstehen asynchron und lassen sich so nicht zuordnen (Grenze).
func Created(a Access, res platform.Result) (resource, id string, ok bool) {
	if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return "", "", false
	}
	if a.Action == Run && a.Resource == Training { // Start und Stopp melden ihre Aufgabe (Review 5, M1)
		if m := taskLoc.FindStringSubmatch(res.Location); m != nil {
			return Task, canonID(m[1]), true
		}
		return "", "", false
	}
	if a.Action != Create {
		return "", "", false
	}
	switch a.Resource {
	case Dataset, Model:
		if id := bodyID([]byte(res.Body), "id"); id != "" {
			return a.Resource, id, true
		}
	case Training:
		if m := taskLoc.FindStringSubmatch(res.Location); m != nil {
			return Task, canonID(m[1]), true
		}
	}
	return "", "", false
}

var taskLoc = regexp.MustCompile(`^/tasks/([0-9]+)(?:$|\?)`)

// Resources listet die bekannten Ressourcen (für Hilfetexte).
func Resources() []string {
	out := make([]string, 0, len(resources))
	for k := range resources {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
